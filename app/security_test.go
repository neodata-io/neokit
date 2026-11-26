package app_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
)

func newAppWith(t *testing.T, o app.Options) *app.App {
	t.Helper()
	o.Name, o.Log = "testapp", quiet()
	if o.Base.LogLevel == "" {
		o.Base.LogLevel, o.Base.LogFormat = "error", "json"
	}
	a, err := app.New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func request(t *testing.T, a *app.App, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.HTTP.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	return resp
}

// A service that did nothing but call New must already answer with the headers
// that keep a browser from rendering, framing or leaking its responses — on the
// routes it mounted, on the errors they return, and on the 404 for one it did
// not.
func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	a := newApp(t)
	a.API.Get("/ok", func(c fiber.Ctx) error { return c.SendString("ok") })
	a.API.Get("/boom", func(fiber.Ctx) error { return fiber.NewError(fiber.StatusConflict, "no") })

	for _, path := range []string{a.APIBase + "/ok", a.APIBase + "/boom", "/nowhere"} {
		resp := request(t, a, http.MethodGet, path, nil)
		for name, want := range map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
			"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		} {
			if got := resp.Header.Get(name); got != want {
				t.Errorf("GET %s: %s = %q, want %q", path, name, got, want)
			}
		}
	}
}

func TestSecurityHookAdjustsTheHeaders(t *testing.T) {
	a := newAppWith(t, app.Options{Security: func(s *app.Security) {
		s.Headers.ContentSecurityPolicy = "default-src 'self'"
		s.Headers.ResourcePolicy = "same-site"
	}})
	resp := request(t, a, http.MethodGet, "/", nil)

	if got := resp.Header.Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("Content-Security-Policy = %q", got)
	}
	if got := resp.Header.Get("Cross-Origin-Resource-Policy"); got != "same-site" {
		t.Errorf("Cross-Origin-Resource-Policy = %q", got)
	}
}

// The case the guard exists for: a page on some other site makes the visitor's
// browser write to this API. No login is configured in this test, and that is
// deliberate — an open API on a private network has nothing else in the way.
func TestAnotherSiteCannotWriteThroughAVisitorsBrowser(t *testing.T) {
	a := newApp(t)
	hit := false
	a.API.Post("/lights/off", func(c fiber.Ctx) error { hit = true; return c.SendStatus(fiber.StatusNoContent) })

	resp := request(t, a, http.MethodPost, a.APIBase+"/lights/off", map[string]string{
		"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example",
	})
	if resp.StatusCode != http.StatusForbidden || hit {
		t.Fatalf("status = %d, handler ran = %v; want 403 and no handler", resp.StatusCode, hit)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"cross_origin"`) {
		t.Errorf("body = %s, want the envelope with the cross_origin code", body)
	}

	// The same request from the API's own pages, and from a script, still works.
	for name, headers := range map[string]map[string]string{
		"own origin": {"Sec-Fetch-Site": "same-origin"},
		"a script":   nil,
	} {
		if resp := request(t, a, http.MethodPost, a.APIBase+"/lights/off", headers); resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s: status = %d, want 204", name, resp.StatusCode)
		}
	}
}

// An origin CORS lets read is an origin that may write: one list, not two that
// have to be kept in step.
func TestACorsOriginMayWrite(t *testing.T) {
	a := newAppWith(t, app.Options{Base: config.Base{CorsOrigins: []string{"https://app.example.com"}}})
	a.API.Post("/things", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	resp := request(t, a, http.MethodPost, a.APIBase+"/things", map[string]string{
		"Sec-Fetch-Site": "cross-site", "Origin": "https://app.example.com",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

// No CORS_ORIGINS means no other site may read the API. Fiber's own reading of
// an empty list is "every origin", which is the opposite.
func TestWithNoCorsOriginsNoOtherSiteMayRead(t *testing.T) {
	a := newApp(t)
	a.API.Get("/things", func(c fiber.Ctx) error { return c.SendString("[]") })

	resp := request(t, a, http.MethodGet, a.APIBase+"/things", map[string]string{"Origin": "https://evil.example"})
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want none", got)
	}
}

func TestCrossOriginWritesCanBeAllowedOnPurpose(t *testing.T) {
	a := newAppWith(t, app.Options{Security: func(s *app.Security) { s.AllowCrossOriginWrites = true }})
	a.API.Post("/things", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	resp := request(t, a, http.MethodPost, a.APIBase+"/things", map[string]string{
		"Sec-Fetch-Site": "cross-site", "Origin": "https://anywhere.example",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

// A frontend on another origin has to be able to send a bearer token and a
// conditional write, and read the headers that answer them. A header missing
// from either list fails as an opaque preflight error.
func TestCorsCoversWhatTheKitItselfUses(t *testing.T) {
	a := newAppWith(t, app.Options{Base: config.Base{CorsOrigins: []string{"https://app.example.com"}}})
	a.API.Get("/things", func(c fiber.Ctx) error { return c.SendString("ok") })

	pre := request(t, a, http.MethodOptions, a.APIBase+"/things", map[string]string{
		"Origin":                         "https://app.example.com",
		"Access-Control-Request-Method":  "PUT",
		"Access-Control-Request-Headers": "authorization, if-match",
	})
	allowed := strings.ToLower(pre.Header.Get("Access-Control-Allow-Headers"))
	for _, h := range []string{"authorization", "if-match", "if-none-match", "content-type"} {
		if !strings.Contains(allowed, h) {
			t.Errorf("Access-Control-Allow-Headers = %q, missing %s", allowed, h)
		}
	}

	resp := request(t, a, http.MethodGet, a.APIBase+"/things", map[string]string{"Origin": "https://app.example.com"})
	exposed := strings.ToLower(resp.Header.Get("Access-Control-Expose-Headers"))
	for _, h := range []string{"etag", "location", "retry-after"} {
		if !strings.Contains(exposed, h) {
			t.Errorf("Access-Control-Expose-Headers = %q, missing %s", exposed, h)
		}
	}
}

// ── Proxies ─────────────────────────────────────────────────────────────────

// ipApp answers with the client address the app believes in.
func ipApp(t *testing.T, proxies ...string) *app.App {
	t.Helper()
	a := newAppWith(t, app.Options{Base: config.Base{TrustedProxies: proxies}})
	a.API.Get("/ip", func(c fiber.Ctx) error { return c.SendString(c.IP() + " " + c.Scheme()) })
	return a
}

func clientSeen(t *testing.T, a *app.App, headers map[string]string) string {
	t.Helper()
	body, _ := io.ReadAll(request(t, a, http.MethodGet, a.APIBase+"/ip", headers).Body)
	return string(body)
}

// With no proxy named, nothing a caller sends changes who the app thinks it is.
// (0.0.0.0 is the peer address of Fiber's in-memory test connection.)
func TestForwardedHeadersAreIgnoredWithoutATrustedProxy(t *testing.T) {
	got := clientSeen(t, ipApp(t), map[string]string{
		"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "https",
	})
	if got != "0.0.0.0 http" {
		t.Errorf("client = %q, want the connection's own peer over http", got)
	}
}

// With the proxy named, the address is the last one that proxy vouches for —
// not the first entry in the header, which is whatever the client typed.
func TestATrustedProxyDecidesTheClientAddress(t *testing.T) {
	got := clientSeen(t, ipApp(t, "0.0.0.0"), map[string]string{
		"X-Forwarded-For": "6.6.6.6, 203.0.113.9", "X-Forwarded-Proto": "https",
	})
	if got != "203.0.113.9 https" {
		t.Errorf("client = %q, want %q: a spoofed leading entry must not win", got, "203.0.113.9 https")
	}
}

// A proxy that is named but is not the peer is not believed either.
func TestAnUnlistedPeerIsNotBelieved(t *testing.T) {
	got := clientSeen(t, ipApp(t, "10.0.0.0/8"), map[string]string{
		"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "https",
	})
	if got != "0.0.0.0 http" {
		t.Errorf("client = %q, want the connection's own peer over http", got)
	}
}

// A typo must not start a server that is wrong about every client address.
func TestAMalformedTrustedProxyFailsAtBoot(t *testing.T) {
	for _, bad := range []string{"proxy.internal", "10.0.0.0/33", "10.0.0", ""} {
		_, err := app.New(app.Options{
			Name: "testapp", Log: quiet(),
			Base: config.Base{TrustedProxies: []string{"10.0.0.1", bad}},
		})
		if err == nil {
			t.Errorf("TRUSTED_PROXIES entry %q was accepted", bad)
		}
	}
}

// ── Request bounds ──────────────────────────────────────────────────────────

// 4 KB of headers is one single-sign-on cookie behind a shared proxy; the
// default has to clear that, and the Fiber hook has to stay the last word.
func TestHeaderBufferDefaultsAboveFibersAndStaysOverridable(t *testing.T) {
	if got := newApp(t).HTTP.Config().ReadBufferSize; got != 16<<10 {
		t.Errorf("ReadBufferSize = %d, want %d", got, 16<<10)
	}
	a := newAppWith(t, app.Options{Fiber: func(c *fiber.Config) { c.ReadBufferSize = 32 << 10 }})
	if got := a.HTTP.Config().ReadBufferSize; got != 32<<10 {
		t.Errorf("ReadBufferSize = %d after the hook, want %d", got, 32<<10)
	}
}
