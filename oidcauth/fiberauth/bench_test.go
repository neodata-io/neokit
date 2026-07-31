package fiberauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/oidcauth"
)

// The claim these benchmarks exist to hold is the one in the package doc: an
// application that has not configured a login pays nothing for having the gate
// mounted.
//
// "Nothing" is measurable here — the disabled middleware must not allocate, must
// not read a cookie, and must not reach the session store. Run:
//
//	go test ./oidcauth/fiberauth -bench 'Middleware|Guard' -benchmem
//
// BenchmarkResolveIdentityDisabled is expected at 0 allocs/op; the enabled
// no-cookie path is the honest comparison, since that is the overwhelmingly
// common request on a deployment that *does* have a login.

// benchApp mounts the middleware in front of a trivial handler.
func benchApp(g *Gate, mw fiber.Handler) *fiber.App {
	app := fiber.New()
	app.Use(mw)
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })
	return app
}

func runBench(b *testing.B, app *fiber.App, req func() *http.Request) {
	b.ReportAllocs()
	for b.Loop() {
		resp, err := app.Test(req(), fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			b.Fatal(err)
		}
		_ = resp.Body.Close()
	}
}

// The zero-cost case: no login configured. The middleware must return on its
// first branch.
func BenchmarkResolveIdentityDisabled(b *testing.B) {
	g := New(Options{
		Provider: func() *oidcauth.Provider { return nil },
		Sessions: newMemStore(), CookiePrefix: "myapp", RateLimit: -1,
	})
	app := benchApp(g, g.ResolveIdentity())
	runBench(b, app, func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) })
}

// Even with a login configured, a request carrying no session cookie must not
// reach the store — this is most requests on a public surface.
func BenchmarkResolveIdentityEnabledNoCookie(b *testing.B) {
	p, _ := oidcauth.New(oidcauth.Config{
		Issuer: "https://id.example.com", ClientID: "app", ClientSecret: "s",
		BaseURL: "https://app.example.com",
	})
	g := New(Options{
		Provider: func() *oidcauth.Provider { return p },
		Sessions: newMemStore(), CookiePrefix: "myapp", RateLimit: -1,
	})
	app := benchApp(g, g.ResolveIdentity())
	runBench(b, app, func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) })
}

// The authenticated steady state: a live session, recently seen, so the store is
// read once and not written.
func BenchmarkResolveIdentityLiveSession(b *testing.B) {
	p, _ := oidcauth.New(oidcauth.Config{
		Issuer: "https://id.example.com", ClientID: "app", ClientSecret: "s",
		BaseURL: "http://app.example.com",
	})
	store := newMemStore()
	store.put("tok", liveSession(true))
	g := New(Options{
		Provider: func() *oidcauth.Provider { return p },
		Sessions: store, CookiePrefix: "myapp", RateLimit: -1,
	})
	app := benchApp(g, g.ResolveIdentity())
	runBench(b, app, func() *http.Request {
		return withCookie(httptest.NewRequest(http.MethodGet, "/", nil), "myapp_session", "tok")
	})
}

// A disabled guard is a straight pass-through; it must cost no more than the
// bare handler.
func BenchmarkGuardDisabled(b *testing.B) {
	g := New(Options{
		Provider: func() *oidcauth.Provider { return nil },
		Sessions: newMemStore(), CookiePrefix: "myapp", RateLimit: -1,
	})
	app := benchApp(g, g.RequireOwner())
	runBench(b, app, func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) })
}

// The baseline: the same app with no auth middleware at all. The gap between
// this and BenchmarkResolveIdentityDisabled is the true cost of mounting a gate
// nobody uses.
func BenchmarkBaselineNoMiddleware(b *testing.B) {
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })
	runBench(b, app, func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) })
}

// HashToken runs once per authenticated request, so its cost sits on the hot
// path even though it is only a sha256 of a short token.
func BenchmarkHashToken(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = oidcauth.HashToken("VGhpcyBpcyBhIHRlc3Qgc2Vzc2lvbiB0b2tlbg")
	}
}

// The open-redirect guard runs on every login and every callback.
func BenchmarkSafeReturnPath(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = SafeReturnPath("/dashboard/energy?tab=today")
	}
}
