package httpc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// This file owns the one guard a plugin needs when it fetches a URL that a
// *caller* influenced — cover art referenced by an upstream payload, a poster
// path echoed back through an opaque id. Those fetches leave the host, carry the
// host's network position, and hand the response body back to a browser, which
// is the whole of SSRF: an unauthenticated caller borrowing NeoGate's LAN access
// to read something it could not reach itself.
//
// A caller-side allowlist check is the strict answer and the right default: the artwork is a
// path on a service the plugin already talks to, so nothing absolute is ever
// legitimate. Use it whenever it fits.
//
// This file is the answer for the case where it does not. Some upstreams
// genuinely hand back absolute artwork URLs on public CDNs — BluOS returns one
// for every streaming source, so rejecting absolute URLs outright would blank
// the now-playing tile. There the URL cannot be constrained by shape, only by
// destination, and the check has to happen against the *resolved* address.

// IsPrivateAddr reports whether ip is in one of the address classes an SSRF
// payload aims at: RFC1918/ULA private space, loopback, link-local (which
// includes 169.254.169.254, the cloud metadata endpoint), the unspecified
// address, and multicast. A public CDN never resolves to one of these.
//
// It is exported because the decision is a policy a plugin may need to apply
// itself — at dial time, prefer [SSRFGuardedTransport], which applies it for you
// without the TOCTOU gap.
func IsPrivateAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsPrivate() ||
		ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// SSRFGuardedTransport returns a base RoundTripper that refuses to complete a
// connection to a private or otherwise internal address.
//
// The check runs in DialContext against conn.RemoteAddr — the address actually
// connected to — rather than against the hostname in the URL. That ordering is
// the point: a name that resolves to a public address on the first lookup and a
// private one on the second (DNS rebinding) defeats any check made before the
// dial, and a resolve-then-compare check has the same gap. By the time this
// inspects the socket there is nothing left to re-resolve.
//
// Pass it as [HTTPOptions.Transport] so the retry and otel layers still wrap it:
//
//	http: neogate.NewHTTPClient(neogate.HTTPOptions{Transport: neogate.SSRFGuardedTransport()})
func SSRFGuardedTransport() *http.Transport {
	dialer := &net.Dialer{}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			host, _, splitErr := net.SplitHostPort(conn.RemoteAddr().String())
			if splitErr != nil {
				conn.Close()
				return nil, fmt.Errorf("ssrf guard: unparsable remote address %q", conn.RemoteAddr())
			}
			ip, parseErr := netip.ParseAddr(host)
			if parseErr != nil {
				conn.Close()
				return nil, fmt.Errorf("ssrf guard: unparsable remote address %q", host)
			}
			if IsPrivateAddr(ip) {
				conn.Close()
				return nil, fmt.Errorf("ssrf guard: refusing to fetch a private/internal address (%s)", ip)
			}
			return conn, nil
		},
	}
}

// SafeAbsoluteArtURL reports whether raw is an absolute artwork URL worth
// handing to an SSRF-guarded client: http or https, with a host, and no
// credentials. It is a shape check only — it says nothing about where the host
// resolves, which is [SSRFGuardedTransport]'s job and cannot be answered here
// without reintroducing the rebinding gap this package is careful to avoid.
//
// Reject-by-shape first anyway: it costs nothing and it keeps schemes like
// file:, gopher: and ftp: from ever reaching a dialer.
func SafeAbsoluteArtURL(raw string) bool {
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
