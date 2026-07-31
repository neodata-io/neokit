package app_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/lifecycle"
)

// The teardown order the builder produces. The application's own steps sit
// between the builder's, and the whole thing is the build order backwards —
// nothing is closed while something depending on it is still alive.
func TestTeardownOrder(t *testing.T) {
	var ran []string
	var shut lifecycle.Stack
	shut.Log = quiet()

	// What New pushes, in order.
	for _, n := range []string{"tracing", "metrics-export"} {
		shut.Push(n, func(context.Context) error { ran = append(ran, n); return nil })
	}
	// What the application pushes.
	for _, n := range []string{"database", "workers"} {
		shut.Push(n, func(context.Context) error { ran = append(ran, n); return nil })
	}
	// What Run pushes.
	for _, n := range []string{"metrics-server", "background-context", "api", "streams"} {
		shut.Push(n, func(context.Context) error { ran = append(ran, n); return nil })
	}
	if err := shut.Shutdown(context.Background(), time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	at := func(name string) int {
		i := slices.Index(ran, name)
		if i < 0 {
			t.Fatalf("step %q never ran: %v", name, ran)
		}
		return i
	}
	// Streams are released before the drain, or the drain waits them out.
	if at("streams") > at("api") {
		t.Error(`"streams" must precede "api"`)
	}
	// The context is cancelled after the drain: reversing them lets a late
	// request start background work concurrently with the wait for it.
	if at("background-context") < at("api") {
		t.Error(`"background-context" must follow "api"`)
	}
	// The application's own resources outlive the HTTP server.
	for _, n := range []string{"api", "background-context"} {
		if at(n) > at("database") {
			t.Errorf("%q runs after \"database\" — a query would hit a closed store", n)
		}
	}
	// Traces flush last, so a span from any earlier step still exports.
	if at("tracing") != len(ran)-1 {
		t.Errorf(`"tracing" must be last: %v`, ran)
	}
}

// Close covers the early-return paths while Run covers the normal one. Both fire
// on a normal boot, so the second must be inert — a store closed twice, or a
// channel closed twice, panics at exit.
func TestCloseIsIdempotent(t *testing.T) {
	a := newApp(t)
	runs := 0
	a.Shutdown.Push("thing", func(context.Context) error { runs++; return nil })

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close must be inert: %v", err)
	}
	if runs != 1 {
		t.Errorf("step ran %d times, want 1", runs)
	}
}

// A listener that cannot bind is fatal and must surface from Run, so the process
// exits non-zero rather than sitting there serving nothing.
func TestRunReturnsAFatalListenerError(t *testing.T) {
	// Occupy a port so the application listener cannot have it.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	a, err := app.New(app.Options{
		Name: "testapp", Log: quiet(), Banner: boolPtr(false),
		Base: config.Base{Port: port, BindAddr: "127.0.0.1", MetricsPort: 0,
			LogLevel: "error", LogFormat: "json"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- a.Run() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run must return the bind failure")
		}
		// netx.AddrInUseHint names the variable to change.
		if !strings.Contains(err.Error(), "PORT") {
			t.Errorf("err = %v, want it to name PORT", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after a failed bind")
	}
}

func boolPtr(b bool) *bool { return &b }

// The report is what the process says it is. It must name every declared
// subsystem and mark each on or off.
func TestReportNamesEverySubsystem(t *testing.T) {
	a := newApp(t)
	a.Declare(app.Subsystem{Name: "database", On: true, Detail: "./data/app.db"})
	a.Declare(app.Subsystem{Name: "login", On: false, Detail: "not configured"})

	got := a.Report(":8080")
	for _, want := range []string{"database", "./data/app.db", "login", "not configured", "testapp"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "✓") || !strings.Contains(got, "✗") {
		t.Errorf("report must mark on and off:\n%s", got)
	}
}

// Draining is closed before the drain so long-lived handlers can return.
func TestDrainingClosesOnShutdown(t *testing.T) {
	a := newApp(t)
	select {
	case <-a.Draining:
		t.Fatal("Draining must be open while the app runs")
	default:
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-a.Draining:
	case <-time.After(time.Second):
		t.Error("Draining must be closed by shutdown")
	}
}

// A failing teardown step must reach the caller, or a process exits 0 having
// failed to flush.
func TestCloseSurfacesAStepError(t *testing.T) {
	a := newApp(t)
	boom := errors.New("flush failed")
	a.Shutdown.Push("thing", func(context.Context) error { return boom })

	if err := a.Close(); !errors.Is(err, boom) {
		t.Errorf("Close err = %v, want %v", err, boom)
	}
}

// The standard chain runs on every request, so its cost over a bare Fiber app is
// the number that matters. Compare against BenchmarkBareFiber.
func BenchmarkStandardChain(b *testing.B) {
	a, err := app.New(app.Options{
		Name: "bench", Log: quiet(), Banner: boolPtr(false),
		Base: config.Base{LogLevel: "error", LogFormat: "json"},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer a.Close()
	a.Fiber.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	b.ReportAllocs()
	for b.Loop() {
		resp, err := a.Fiber.Test(httptest.NewRequest(http.MethodGet, "/", nil),
			fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			b.Fatal(err)
		}
		_ = resp.Body.Close()
	}
}

func BenchmarkBareFiber(b *testing.B) {
	f := fiber.New()
	f.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	b.ReportAllocs()
	for b.Loop() {
		resp, err := f.Test(httptest.NewRequest(http.MethodGet, "/", nil),
			fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			b.Fatal(err)
		}
		_ = resp.Body.Close()
	}
}
