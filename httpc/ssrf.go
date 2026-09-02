package httpc

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
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
// payload aims at: RFC1918/ULA private space, loopback, link-local (which
// includes 169.254.169.254, the cloud metadata endpoint), the unspecified
// address, and multicast. A public CDN never resolves to one of these.
//
// This is the strict policy, and the right default. Use it whenever the fetch
// has no business leaving the public internet.
//
// It is exported because the decision is a policy a caller may need to apply
// itself — at dial time, prefer [GuardedTransport], which applies it for you
// without the TOCTOU gap.
func IsPrivateAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsPrivate() ||
		IsInternalAddr(ip)
}

// IsInternalAddr reports whether ip means "this machine" or "this link" rather
// than "a host on the network": loopback, link-local (169.254.169.254 among
// them), the unspecified address, and multicast.
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
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// SSRFGuardedTransport returns a base RoundTripper that refuses to connect to a
// private or otherwise internal address. It is [GuardedTransport] with the
// strict [IsPrivateAddr] policy.
func SSRFGuardedTransport() *http.Transport { return GuardedTransport(IsPrivateAddr) }

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
// The returned transport is a plain *http.Transport, so a caller that needs one
// more thing can set it — a LAN probe tolerating self-signed certificates, say:
//
//	tr := httpc.GuardedTransport(httpc.IsInternalAddr)
//	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
//
// Pass it as [HTTPOptions.Transport] so the retry and otel layers still wrap it:
//
//	http: httpc.NewHTTPClient(httpc.HTTPOptions{Transport: httpc.SSRFGuardedTransport()})
func GuardedTransport(deny Policy) *http.Transport {
	if deny == nil {
		deny = IsPrivateAddr
	}
	dialer := &net.Dialer{
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("ssrf guard: unparsable address %q", address)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("ssrf guard: unparsable address %q", host)
			}
			if deny(ip) {
				return fmt.Errorf("ssrf guard: refusing to fetch a private/internal address (%s)", ip)
			}
			return nil
		},
	}
	return &http.Transport{DialContext: dialer.DialContext}
}

// SafeAbsoluteURL reports whether raw is an absolute URL worth handing to an
// SSRF-guarded client: http or https, with a host, and no embedded
// credentials. It is a shape check only — it says nothing about where the host
// resolves, which is [SSRFGuardedTransport]'s job and cannot be answered here
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
