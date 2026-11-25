package fiberx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

func run(t *testing.T, app *fiber.App, req *http.Request) *http.Response {
	t.Helper()
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

// ── Response headers ────────────────────────────────────────────────────────

func headerApp(h SecurityHeaders, cfg ...fiber.Config) *fiber.App {
	app := fiber.New(cfg...)
	app.Use(h.Middleware())
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/boom", func(fiber.Ctx) error { return fiber.NewError(fiber.StatusTeapot, "no") })
	app.Get("/page", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderContentSecurityPolicy, "default-src 'self'")
		return c.SendString("<html>")
	})
	return app
}

func TestDefaultHeadersAreOnEveryAnswer(t *testing.T) {
	app := headerApp(DefaultSecurityHeaders())
	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
	}
	// An error is the answer most likely to be opened in a tab and least likely to
	// have been thought about, so it is checked alongside the success.
	for _, path := range []string{"/", "/boom", "/nowhere"} {
		resp := run(t, app, httptest.NewRequest(http.MethodGet, path, nil))
		for name, value := range want {
			if got := resp.Header.Get(name); got != value {
				t.Errorf("GET %s: %s = %q, want %q", path, name, got, value)
			}
		}
	}
}

// The defaults must not include the two headers that break an ordinary
// deployment when sent unasked.
func TestDefaultsLeaveOutWhatADeploymentMustChoose(t *testing.T) {
	resp := run(t, headerApp(DefaultSecurityHeaders()), httptest.NewRequest(http.MethodGet, "/", nil))
	for _, name := range []string{"Cross-Origin-Resource-Policy", "Strict-Transport-Security"} {
		if got := resp.Header.Get(name); got != "" {
			t.Errorf("%s = %q by default, want it absent", name, got)
		}
	}
}

func TestAnEmptyFieldLeavesItsHeaderOut(t *testing.T) {
	h := DefaultSecurityHeaders()
	h.ContentSecurityPolicy = ""
	h.FrameOptions = "  "
	resp := run(t, headerApp(h), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Errorf("Content-Security-Policy = %q, want absent", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "" {
		t.Errorf("X-Frame-Options = %q, want absent", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff must survive every configuration, got %q", got)
	}
}

// A route that serves a page sets its own policy; the middleware runs first, so
// the handler's value is the one that leaves.
func TestAHandlerMayReplaceAHeader(t *testing.T) {
	resp := run(t, headerApp(DefaultSecurityHeaders()), httptest.NewRequest(http.MethodGet, "/page", nil))
	if got := resp.Header.Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("Content-Security-Policy = %q, want the handler's", got)
	}
}

// HSTS on a plain-HTTP answer is ignored by browsers and misleading to readers,
// so it rides only on a request that arrived over HTTPS.
func TestHSTSIsSentOnlyOverHTTPS(t *testing.T) {
	h := DefaultSecurityHeaders()
	h.HSTS = 180 * 24 * time.Hour
	// Trust the test peer as a proxy, so X-Forwarded-Proto decides the scheme the
	// way it does behind a TLS-terminating proxy.
	app := headerApp(h, fiber.Config{
		TrustProxy:       true,
		TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0"}},
	})

	plain := run(t, app, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := plain.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("over HTTP: Strict-Transport-Security = %q, want absent", got)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	secure := run(t, app, req)
	if got := secure.Header.Get("Strict-Transport-Security"); got != "max-age=15552000" {
		t.Errorf("over HTTPS: Strict-Transport-Security = %q, want max-age=15552000", got)
	}
}

// ── Cross-origin writes ─────────────────────────────────────────────────────

func guardApp(trusted ...string) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: NewErrors(nil).Render})
	app.Use(CrossOriginGuard(trusted))
	ok := func(c fiber.Ctx) error { return c.SendString("ok") }
	app.Get("/", ok)
	app.Post("/", ok)
	app.Put("/", ok)
	app.Patch("/", ok)
	app.Delete("/", ok)
	app.Options("/", ok)
	return app
}

func guardRequest(method string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, "http://api.example.com/", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func TestCrossOriginGuard(t *testing.T) {
	const trusted = "https://app.example.com"

	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		// Not a browser: no ambient credentials to borrow, so nothing to forge.
		{"a script with neither header", http.MethodPost, nil, 200},

		// What the browser says about its own request is decisive.
		{"same-origin fetch", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://api.example.com"}, 200},
		{"typed or bookmarked", http.MethodPost, map[string]string{"Sec-Fetch-Site": "none"}, 200},
		{"another site", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		// Lax cookies travel on this one, which is why the guard exists at all.
		{"a sibling subdomain", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://other.example.com"}, 403},
		{"another site with no Origin", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"a trusted frontend, cross-site", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": trusted}, 200},
		{"a trusted frontend, same-site", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": trusted}, 200},

		// An older browser sends Origin alone.
		{"old browser, own host", http.MethodPost, map[string]string{"Origin": "https://api.example.com"}, 200},
		{"old browser, another site", http.MethodPost, map[string]string{"Origin": "https://evil.example"}, 403},
		{"old browser, trusted frontend", http.MethodPost, map[string]string{"Origin": trusted}, 200},
		{"a sandboxed frame", http.MethodPost, map[string]string{"Origin": "null"}, 403},
		// A host that merely starts with ours is a different host.
		{"old browser, lookalike host", http.MethodPost, map[string]string{"Origin": "https://api.example.com.evil.example"}, 403},

		// Every writing method is covered, not just POST.
		{"cross-site PUT", http.MethodPut, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"cross-site PATCH", http.MethodPatch, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"cross-site DELETE", http.MethodDelete, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},

		// Reads and preflights pass: a read changes nothing, and refusing a preflight
		// would turn every CORS failure into this one.
		{"cross-site GET", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 200},
		{"cross-site preflight", http.MethodOptions, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 200},
	}

	app := guardApp(trusted)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := run(t, app, guardRequest(tc.method, tc.headers))
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// The refusal says what it is: "forbidden" sends a developer to their
// permissions when the answer is in their origin list.
func TestCrossOriginRefusalNamesItself(t *testing.T) {
	resp := run(t, guardApp(), guardRequest(http.MethodPost,
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}))
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"code":"cross_origin"`) {
		t.Errorf("body = %s, want the cross_origin code", body)
	}
}

// "Any site may read" is a common CORS setting. It must not become "any site
// may write with the visitor's cookies".
func TestAWildcardOriginIsNotTrustedToWrite(t *testing.T) {
	resp := run(t, guardApp("*"), guardRequest(http.MethodPost,
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403: a wildcard read origin must not open writes", resp.StatusCode)
	}
}

// Origins are compared as a browser sends them; a trailing slash in a config
// file is the usual way the two differ.
func TestTrustedOriginsTolerateATrailingSlash(t *testing.T) {
	resp := run(t, guardApp(" https://app.example.com/ "), guardRequest(http.MethodPost,
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://app.example.com"}))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// ── Created ─────────────────────────────────────────────────────────────────

func TestCreatedNamesTheNewResource(t *testing.T) {
	app := fiber.New()
	app.Post("/api/v1/links/", func(c fiber.Ctx) error {
		return Created(c, MemberURL(c, "a/b"), fiber.Map{"id": "a/b"})
	})

	resp := run(t, app, httptest.NewRequest(http.MethodPost, "/api/v1/links/", nil))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	// One slash between collection and member, and the id escaped: a reserved
	// character in an id must not become part of the path's structure.
	if got, want := resp.Header.Get("Location"), "/api/v1/links/a%2Fb"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

// ── Cost ────────────────────────────────────────────────────────────────────

// benchHandler drives the app's handler directly, with no connection and no
// request parsing in the loop: app.Test costs several microseconds and thirty
// allocations of its own, which would bury a middleware that costs neither.
func benchHandler(b *testing.B, app *fiber.App, method string, headers map[string]string) {
	b.Helper()
	handler := app.Handler()
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI("/")
	for k, v := range headers {
		ctx.Request.Header.Set(k, v)
	}

	b.ReportAllocs()
	for b.Loop() {
		ctx.Response.Reset()
		handler(&ctx)
	}
	if got := ctx.Response.StatusCode(); got != fiber.StatusOK {
		b.Fatalf("status = %d: the benchmark measured a refusal, not the pass", got)
	}
}

// Both run on every request, so what they cost over a bare app is the number
// that matters. Compare against BenchmarkBare.
func BenchmarkBare(b *testing.B) {
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	benchHandler(b, app, http.MethodGet, nil)
}

func BenchmarkSecurityHeaders(b *testing.B) {
	app := fiber.New()
	app.Use(DefaultSecurityHeaders().Middleware())
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	benchHandler(b, app, http.MethodGet, nil)
}

func BenchmarkCrossOriginGuard_Write(b *testing.B) {
	app := fiber.New()
	app.Use(CrossOriginGuard([]string{"https://app.example.com"}))
	app.Post("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	benchHandler(b, app, http.MethodPost, map[string]string{
		"Sec-Fetch-Site": "cross-site", "Origin": "https://app.example.com",
	})
}
