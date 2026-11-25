package httpc

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// maxRedirects matches net/http's own limit, which a custom CheckRedirect replaces.
const maxRedirects = 10

// errCrossHostBody refuses a 307/308 that would replay the body to another host.
var errCrossHostBody = errors.New("httpc: refusing a redirect that replays the request body to another host")

// crossHostHeaders are the only request headers that follow a redirect to
// another host: negotiation and tracing, nothing that authenticates.
var crossHostHeaders = map[string]bool{
	"Accept":          true,
	"Accept-Encoding": true,
	"Accept-Language": true,
	"Content-Type":    true,
	"User-Agent":      true,
	"Traceparent":     true,
	"Tracestate":      true,
}

// checkRedirect is the redirect policy of every client this package builds.
// net/http strips only Authorization and Cookie across hosts, so an API key in
// a custom header, or the Referer's query string, would otherwise follow.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("stopped after 10 redirects")
	}
	if sameHost(via[0].URL, req.URL) {
		return nil
	}
	if req.Body != nil && req.Body != http.NoBody {
		return errCrossHostBody
	}
	for k := range req.Header {
		if !crossHostHeaders[http.CanonicalHeaderKey(k)] {
			delete(req.Header, k)
		}
	}
	return nil
}

// sameHost reports whether a redirect stays on the host the request was built
// for. Another port is another service; only the http→https upgrade is exempt.
func sameHost(from, to *url.URL) bool {
	if !strings.EqualFold(from.Hostname(), to.Hostname()) {
		return false
	}
	if effectivePort(from) == effectivePort(to) {
		return true
	}
	return from.Scheme == "http" && to.Scheme == "https" &&
		effectivePort(from) == "80" && effectivePort(to) == "443"
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
