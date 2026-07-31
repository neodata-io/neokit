package fiberx

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// quietLogger keeps the middleware's slog call on a handler that formats
// nothing, so these benchmarks measure fiberx's own per-request overhead rather
// than the cost of writing JSON to a file.
func quietLogger(b *testing.B, level slog.Level) {
	b.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: level})))
	b.Cleanup(func() { slog.SetDefault(prev) })
}

// benchApp builds a Fiber app with the middleware under test and one trivial
// route, then returns a func that drives one request through it.
func benchApp(b *testing.B, level slog.Level) (*fiber.App, *httptest.ResponseRecorder) {
	b.Helper()
	quietLogger(b, level)
	app := fiber.New()
	app.Use(NewErrors(nil).MetricsAndLogger())
	app.Get("/items/:id", func(c fiber.Ctx) error { return c.SendString("ok") })
	return app, nil
}

// BenchmarkMetricsAndLogger_200 is the per-request cost on the happy path: the
// Prometheus counter/histogram writes plus the summary log line. This runs on
// every single request an application serves.
func BenchmarkMetricsAndLogger_200(b *testing.B) {
	app, _ := benchApp(b, slog.LevelInfo)
	req := httptest.NewRequest("GET", "/items/42", nil)
	b.ReportAllocs()
	for b.Loop() {
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}

// BenchmarkMetricsAndLogger_Quiet exercises the skipLog path — a request whose
// summary line is suppressed. The Prometheus work is still paid, so the delta
// against the 200 case isolates the cost of the log line itself.
func BenchmarkMetricsAndLogger_Quiet(b *testing.B) {
	quietLogger(b, slog.LevelInfo)
	app := fiber.New()
	e := NewErrors(nil)
	e.QuietPath = func(string) bool { return true }
	app.Use(e.MetricsAndLogger())
	app.Get("/items/:id", func(c fiber.Ctx) error { return c.SendString("ok") })
	req := httptest.NewRequest("GET", "/items/42", nil)
	b.ReportAllocs()
	for b.Loop() {
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}

// BenchmarkBaseline_NoMiddleware is the control: the same app and route with no
// fiberx middleware at all. Everything above should be read as a delta on this.
func BenchmarkBaseline_NoMiddleware(b *testing.B) {
	app := fiber.New()
	app.Get("/items/:id", func(c fiber.Ctx) error { return c.SendString("ok") })
	req := httptest.NewRequest("GET", "/items/42", nil)
	b.ReportAllocs()
	for b.Loop() {
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}

// BenchmarkRateLimiter_Parallel measures the limiter under concurrent load from
// many distinct callers — the shape that decides whether its bookkeeping is a
// throughput ceiling.
func BenchmarkRateLimiter_Parallel(b *testing.B) {
	h := RateLimiter(1_000_000_000)
	app := fiber.New()
	app.Use(h)
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest("GET", "/", nil)
		for pb.Next() {
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 0})
			if err != nil {
				b.Fatal(err)
			}
			resp.Body.Close()
		}
	})
}

// BenchmarkCodeForStatus covers the status→code mapping used on every rendered
// error.
func BenchmarkCodeForStatus(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		strSink = CodeForStatus(404)
	}
}

var strSink string
