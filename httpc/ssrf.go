package httpc

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// This file owns the guard needed when fetching a URL a *caller* influenced.
// Such fetches leave the process carrying its network position and hand the
// response back to a browser — the whole of SSRF: an unauthenticated caller
// borrowing this process's network position to read what it could not reach
// itself.
//
// A caller-side allowlist is the strict answer and the right default; use it
// whenever it fits. This file is for when it does not — some upstreams do hand
// back absolute URLs on public CDNs, and a self-hosted app is routinely pointed
// at a box on the user's own LAN. There the URL can be constrained only by
// destination, checked against the *resolved* address, which is what a [Policy]
// passed to [GuardedTransport] does.

// A Policy reports whether an address is off-limits. It is consulted at dial
// time by [GuardedTransport], against the address actually connected to.
//
// Two stock policies cover the cases that come up: [IsPrivateAddr] for a fetch
// that should only ever reach the public internet, and [IsInternalAddr] for one
// that is legitimately allowed onto the local network.
type Policy func(ip netip.Addr) bool

// IsPrivateAddr reports whether ip is in one of the address classes an SSRF
// payload aims at: RFC1918/ULA private space, carrier-grade NAT, and everything
// [IsInternalAddr] refuses. A public CDN never resolves to one of these.
//
// This is the strict policy, and the right default. Use it whenever the fetch
// has no business leaving the public internet.
//
// It is exported because the decision is a policy a caller may need to apply
// itself — at dial time, prefer [GuardedTransport], which applies it for you
// without the TOCTOU gap.
func IsPrivateAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if v4, ok := embeddedIPv4(ip); ok {
		return IsPrivateAddr(v4)
	}
	return ip.IsPrivate() ||
		inAny(ip, privateRanges) ||
		IsInternalAddr(ip)
}

// IsInternalAddr reports whether ip means "this machine" or "this link" rather
// than "a host on the network": loopback, link-local (169.254.169.254 among
// them), the cloud metadata endpoints that live elsewhere, multicast, and the
// reserved blocks no host has (0.0.0.0/8, 240.0.0.0/4 and broadcast).
//
// It is [IsPrivateAddr] minus the private ranges, for the fetch that is
// *supposed* to reach the local network — a self-hosted app resolving the
// favicon of a box the user pointed it at, or probing a LAN service for
// reachability. There, blocking RFC1918 would block the entire feature, while
// the addresses above are still pure attack surface: an unauthenticated caller
// who can name a URL must not be able to turn the process into a liveness
// oracle for its own loopback, or read the cloud metadata endpoint.
//
// It is the weaker guard, and deliberately so. Reach for it only when reaching
// the LAN is the actual purpose; otherwise [IsPrivateAddr].
func IsInternalAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if v4, ok := embeddedIPv4(ip); ok {
		return IsInternalAddr(v4)
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		inAny(ip, internalRanges)
}

// internalRanges are never "a box on the network": blocks no host has, and the
// metadata endpoints that sit outside link-local.
var internalRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network"
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved, and broadcast
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
	netip.MustParsePrefix("168.63.129.16/32"),   // Azure WireServer
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS IMDS over IPv6
}

// privateRanges are networks the strict policy refuses beyond what
// [netip.Addr.IsPrivate] knows.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT; also tailnets
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
}

var (
	nat64Prefix     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFourPrefix = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 returns the IPv4 destination a NAT64 or 6to4 address stands for.
// Such an address is judged by that destination: a blanket refusal would break
// every public fetch from an IPv6-only host behind DNS64.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	b := ip.As16()
	switch {
	case !ip.Is6():
		return netip.Addr{}, false
	case nat64Prefix.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFourPrefix.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

func inAny(ip netip.Addr, ranges []netip.Prefix) bool {
	for _, r := range ranges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// GuardedTransport returns a base RoundTripper that refuses to connect to any
// address deny reports as off-limits. A nil deny falls back to [IsPrivateAddr]:
// a caller who forgot to choose gets the strict policy, never an unguarded
// transport that looks guarded.
//
// The check runs in the dialer's Control callback, against the *resolved*
// address, before the connection is established — rather than against the
// hostname in the URL. That ordering is the point: a name that resolves to an
// allowed address on the first lookup and a denied one on the second (DNS
// rebinding) defeats any check made before the dial, and a resolve-then-compare
// check has the same gap. By the time Control runs there is nothing left to
// re-resolve, and a denied address is never actually connected to.
//
// It ignores proxy environment variables on purpose: through a proxy the
// address dialled is the proxy's, and the guard would be checking the wrong one.
//
// Build it once and share it — each call returns a transport with its own
// connection pool.
//
// The returned transport is a plain *http.Transport, so a caller that needs one
// more thing can set it — a LAN probe tolerating self-signed certificates, say:
//
//	tr := httpc.GuardedTransport(httpc.IsInternalAddr)
//	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
//
// Pass it as [HTTPOptions.Transport] so the retry and otel layers still wrap it:
//
//	http: httpc.NewHTTPClient(httpc.HTTPOptions{Transport: httpc.GuardedTransport(httpc.IsPrivateAddr)})
func GuardedTransport(deny Policy) *http.Transport {
	// The limits are http.DefaultTransport's. A zero-value Transport has none: it
	// keeps every idle connection forever, one set per host a caller names.
	return &http.Transport{
		DialContext:           guardedDialer(deny).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// errGuard marks every refusal the guard makes, so the retry layer and
// [Classify] can tell it from a transient dial failure.
var errGuard = errors.New("ssrf guard")

func guardedDialer(deny Policy) *net.Dialer {
	if deny == nil {
		deny = IsPrivateAddr
	}
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: unparsable address %q", errGuard, address)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("%w: unparsable address %q", errGuard, host)
			}
			if deny(ip) {
				return fmt.Errorf("%w: refusing to fetch a private/internal address (%s)", errGuard, ip)
			}
			return nil
		},
	}
}

// SafeAbsoluteURL reports whether raw is an absolute URL worth handing to an
// SSRF-guarded client: http or https, with a host, and no embedded
// credentials. It is a shape check only — it says nothing about where the host
// resolves, which is [GuardedTransport]'s job and cannot be answered here
// without reintroducing the rebinding gap this package is careful to avoid.
//
// Reject-by-shape first anyway: it costs nothing and it keeps schemes like
// file:, gopher: and ftp: from ever reaching a dialer.
func SafeAbsoluteURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil {
		return false
	}
	// url.Parse accepts a bare "http:" with no authority; Hostname() is empty
	// there, and an empty host is not something to dial.
	return strings.TrimSpace(u.Hostname()) != ""
}
