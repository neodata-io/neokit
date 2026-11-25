package fiberx

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

// This file is the browser-facing half of an API's defences: the response
// headers that tell a browser how little to do with an answer, and the request
// check that stops another site from using a visitor's browser to write to this
// one. Neither depends on a login being configured — an API that ships open on a
// private network needs the second one more than a closed API does, because a
// page on the public internet can reach it through any browser on that network.

// SecurityHeaders are the response headers sent with every answer. An empty
// field leaves that header out; start from [DefaultSecurityHeaders] and change
// what differs.
//
// `X-Content-Type-Options: nosniff` is not among the fields because it has no
// setting worth choosing: without it a browser may decide an uploaded file or a
// JSON body "looks like" HTML and run it.
type SecurityHeaders struct {
	// ContentSecurityPolicy governs what a response may load and who may frame it,
	// should a browser ever render one as a document — an error body opened in a
	// tab, a stored SVG, a reflected string. The default allows nothing at all,
	// which is right for an API and wrong for a service that also serves pages:
	// such a service sets its own policy on those routes, or here.
	ContentSecurityPolicy string

	// FrameOptions is the pre-CSP spelling of frame-ancestors, kept for the
	// browsers and embedded web views that still only read this one.
	FrameOptions string

	// ReferrerPolicy decides what the browser tells the next site about the URL it
	// came from. API URLs carry ids and sometimes tokens, so by default none of it
	// leaves.
	ReferrerPolicy string

	// ResourcePolicy is Cross-Origin-Resource-Policy: which sites may embed a
	// response as an image, script or media element. Empty by default, because a
	// frontend on another origin showing this API's images is an ordinary
	// arrangement that "same-origin" would break silently. Set "same-origin" or
	// "same-site" when nothing off-site should embed anything.
	ResourcePolicy string

	// HSTS is how long a browser must refuse plain HTTP for this host. Zero sends
	// nothing, and that is the default on purpose: the header outlives a mistake by
	// its whole max-age, and the proxy terminating TLS is usually where it is
	// already decided. It is only ever sent on a request that arrived over HTTPS —
	// a browser ignores it on plain HTTP, and behind a proxy "arrived over HTTPS"
	// is only knowable when that proxy is trusted (see TRUSTED_PROXIES in
	// [github.com/neodata-io/neokit/config.Base]).
	HSTS time.Duration
}

// DefaultSecurityHeaders is the set for a JSON API: render nothing, be framed by
// no one, tell no other site where the visitor came from.
func DefaultSecurityHeaders() SecurityHeaders {
	return SecurityHeaders{
		ContentSecurityPolicy: "default-src 'none'; frame-ancestors 'none'",
		FrameOptions:          "DENY",
		ReferrerPolicy:        "no-referrer",
	}
}

// Middleware sets the headers before the handler runs, so they are also on an
// error the handler returns, and a handler that knows better — a route serving a
// page with its own policy — simply sets the header again.
//
// The header list is built once, here; a request pays for a handful of map
// writes and no allocation.
func (h SecurityHeaders) Middleware() fiber.Handler {
	type header struct{ name, value string }
	always := []header{{fiber.HeaderXContentTypeOptions, "nosniff"}}
	for _, kv := range []header{
		{fiber.HeaderContentSecurityPolicy, h.ContentSecurityPolicy},
		{fiber.HeaderXFrameOptions, h.FrameOptions},
		{fiber.HeaderReferrerPolicy, h.ReferrerPolicy},
		{"Cross-Origin-Resource-Policy", h.ResourcePolicy},
	} {
		if v := strings.TrimSpace(kv.value); v != "" {
			always = append(always, header{kv.name, v})
		}
	}

	hsts := ""
	if h.HSTS > 0 {
		hsts = "max-age=" + strconv.FormatInt(int64(h.HSTS/time.Second), 10)
	}

	return func(c fiber.Ctx) error {
		for _, kv := range always {
			c.Set(kv.name, kv.value)
		}
		if hsts != "" && c.Scheme() == "https" {
			c.Set(fiber.HeaderStrictTransportSecurity, hsts)
		}
		return c.Next()
	}
}

// ErrCrossOrigin answers a state-changing request a browser sent on behalf of
// another site. Its own code, because "forbidden" would send a developer looking
// at their permissions when the answer is in their origin list.
var ErrCrossOrigin = &APIError{
	Status: fiber.StatusForbidden, Code: "cross_origin",
	Message: "cross-origin request refused",
}

// CrossOriginGuard refuses state-changing requests that a browser sent from
// another origin, unless that origin is in trusted.
//
// This is the defence against cross-site request forgery, and it needs no token
// and no state. Every current browser labels a request with where it came from
// (`Sec-Fetch-Site`), and has sent `Origin` on cross-origin writes for far
// longer; neither header can be set or removed by a page's script. So the rule
// is the one Go's own net/http.CrossOriginProtection applies:
//
//   - GET, HEAD and OPTIONS pass — they must not change anything, and a
//     preflight has to reach the CORS middleware;
//   - `Sec-Fetch-Site: same-origin` or `none` (typed, bookmarked) passes;
//   - any other value passes only for a trusted origin;
//   - with no `Sec-Fetch-Site` (an older browser), `Origin` must name this host
//     or a trusted origin;
//   - with neither header the caller is not a browser — a script, a native app,
//     another service — and passes, because forgery needs a browser's ambient
//     credentials and such a caller has none to borrow.
//
// It matters most where it looks least needed. A session cookie is already
// SameSite=Lax, which a sibling subdomain still gets through; and an API with
// no login at all, open on a private network, is reachable by any page its
// users visit, with nothing but this check in the way.
//
// trusted is a list of origins as a browser sends them, "https://app.example.com".
// Pass the origins CORS allows: one that may read the API across origins is one
// that is meant to write to it. A "*" entry is ignored rather than honoured —
// letting any site read public data is a common wish, letting any site write
// with a visitor's cookies is not what anyone meant by it.
func CrossOriginGuard(trusted []string) fiber.Handler {
	allow := make(map[string]struct{}, len(trusted))
	for _, o := range trusted {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "" || o == "*" {
			continue
		}
		allow[o] = struct{}{}
	}
	isTrusted := func(origin string) bool {
		_, ok := allow[origin]
		return ok
	}

	return func(c fiber.Ctx) error {
		switch c.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return c.Next()
		}

		origin := c.Get(fiber.HeaderOrigin)
		switch c.Get("Sec-Fetch-Site") {
		case "":
			// An older browser, or not a browser: decided by Origin below.
		case "same-origin", "none":
			return c.Next()
		default:
			if isTrusted(origin) {
				return c.Next()
			}
			return ErrCrossOrigin
		}

		if origin == "" {
			return c.Next()
		}
		// Host only: the scheme this server sees is not reliably the one the browser
		// used once a proxy terminates TLS. "null" (a sandboxed frame, a file) has
		// no host to match and falls through to the refusal.
		if _, host, ok := strings.Cut(origin, "://"); ok && host == c.Host() {
			return c.Next()
		}
		if isTrusted(origin) {
			return c.Next()
		}
		return ErrCrossOrigin
	}
}
