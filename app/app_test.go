package app_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newApp(t *testing.T) *app.App {
	t.Helper()
	a, err := app.New(app.Options{
		Name: "testapp", Version: "1.2.3",
		Base: config.Base{Port: 0, MetricsPort: 0, LogLevel: "error", LogFormat: "json"},
		Log:  quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// Name is what stamps every log line, every span and every metric. Without it
// the process is anonymous in three systems at once, so it is required rather
// than defaulted.
func TestNewRequiresAName(t *testing.T) {
	if _, err := app.New(app.Options{Version: "1.0"}); err == nil {
		t.Fatal("New must reject an empty Name")
	}
}

// Every dependency is an exported field. That is the whole design: there is no
// container and no lookup, so a handler receives what it needs through its own
// constructor.
func TestNewExposesEveryDependency(t *testing.T) {
	a := newApp(t)

	if a.Log == nil || a.HTTP == nil || a.Errors == nil ||
		a.Shutdown == nil || a.Context() == nil {
		t.Fatalf("New left a dependency nil: %+v", a)
	}
	if a.Name != "testapp" || a.Version != "1.2.3" {
		t.Errorf("identity not carried: %q %q", a.Name, a.Version)
	}
}

// The application context must outlive New and only be cancelled at shutdown —
// background work is started from it.
func TestAppContextIsLiveUntilClose(t *testing.T) {
	a := newApp(t)

	select {
	case <-a.Context().Done():
		t.Fatal("Ctx must not be cancelled while the app is running")
	default:
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-a.Context().Done():
	case <-time.After(time.Second):
		t.Error("Close must cancel Ctx so background work stops")
	}
}

// New declares its own subsystems, so a caller reading Subsystems() sees the
// whole process — including the parts neokit switched on or left off. A view
// that showed only the caller's own declarations would disagree with the boot
// report about what this process is.
func TestSubsystemsIncludesTheBuildersOwn(t *testing.T) {
	a := newApp(t)
	for _, name := range []string{"tracing", "metrics export", "diagnostics"} {
		if _, ok := findSubsystem(a, name); !ok {
			t.Errorf("%q missing from Subsystems(): %+v", name, a.Subsystems())
		}
	}
}

// The diagnostics listener binds loopback and leaves pprof off, and both of
// those are silent: a scrape from another container just stops arriving, with
// nothing wrong on this side to find. The report is where an operator gets to
// read the address that was actually used, so it has to be the real one.
func TestTheReportNamesTheDiagnosticsAddressAndWhatIsMountedOnIt(t *testing.T) {
	a, err := app.New(app.Options{
		Name: "testapp",
		Base: config.Base{MetricsPort: 9090},
		Log:  quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	got, ok := findSubsystem(a, "diagnostics")
	if !ok {
		t.Fatalf("no diagnostics line: %+v", a.Subsystems())
	}
	if !strings.Contains(got.Detail, "127.0.0.1:9090") {
		t.Errorf("detail = %q, want the loopback default spelled out", got.Detail)
	}
	if strings.Contains(got.Detail, "pprof") {
		t.Errorf("detail = %q, want no pprof claim when it is not mounted", got.Detail)
	}
	if !strings.Contains(a.Report(), "127.0.0.1:9090") {
		t.Errorf("boot report does not name the diagnostics address:\n%s", a.Report())
	}
}

// Enabling pprof publishes heap dumps on that port. The report must say so —
// it is the only place the decision is visible at runtime.
func TestTheReportSaysWhenPprofIsMounted(t *testing.T) {
	a, err := app.New(app.Options{
		Name: "testapp",
		Base: config.Base{MetricsPort: 9090, MetricsBindAddr: "0.0.0.0", EnablePprof: true},
		Log:  quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	got, _ := findSubsystem(a, "diagnostics")
	if !strings.Contains(got.Detail, "0.0.0.0:9090") || !strings.Contains(got.Detail, "pprof") {
		t.Errorf("detail = %q, want the configured address and pprof named", got.Detail)
	}
}

// findSubsystem looks one up by name.
func findSubsystem(a *app.App, name string) (app.Subsystem, bool) {
	for _, s := range a.Subsystems() {
		if s.Name == name {
			return s, true
		}
	}
	return app.Subsystem{}, false
}

// The error envelope must be installed as Fiber's ErrorHandler, or a returned
// error crosses the wire as plain text the client cannot parse.
func TestReturnedErrorsRenderAsTheEnvelope(t *testing.T) {
	a := newApp(t)
	a.HTTP.Get("/boom", func(fiber.Ctx) error {
		return fiber.NewError(http.StatusTeapot, "i am a teapot")
	})

	resp, err := a.HTTP.Test(httptest.NewRequest(http.MethodGet, "/boom", nil),
		fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"error"`) {
		t.Errorf("body = %s, want the {\"error\",\"code\"} envelope", body)
	}
}

// The caller's own sentinels must reach the envelope.
func TestErrorMapperIsConsulted(t *testing.T) {
	sentinel := errors.New("no such horse")
	a := newApp(t)
	defer a.Close()

	a.Errors.Mapper = func(err error) (int, string, string, bool) {
		if errors.Is(err, sentinel) {
			return http.StatusNotFound, "not found", "not_found", true
		}
		return 0, "", "", false
	}

	a.HTTP.Get("/x", func(fiber.Ctx) error { return sentinel })
	resp, _ := a.HTTP.Test(httptest.NewRequest(http.MethodGet, "/x", nil),
		fiber.TestConfig{Timeout: 5 * time.Second})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want the mapper's 404", resp.StatusCode)
	}
}

// A panicking handler must not take the process down, and the recovery must be
// logged with enough to correlate it — a bare 500 with no stack is the least
// information at exactly the wrong moment.
func TestAPanickingHandlerIsRecovered(t *testing.T) {
	a := newApp(t)
	a.HTTP.Get("/panic", func(fiber.Ctx) error { panic("boom") })

	resp, err := a.HTTP.Test(httptest.NewRequest(http.MethodGet, "/panic", nil),
		fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// SSE must not be compressed: a compressor buffers the writes the stream exists
// to flush immediately.
func TestServerSentEventsAreNotCompressed(t *testing.T) {
	a := newApp(t)
	a.HTTP.Get("/stream", func(c fiber.Ctx) error {
		c.Set("Content-Type", "text/event-stream")
		return c.SendString("data: hello\n\n")
	})

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := a.HTTP.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want none — compression breaks SSE flushing", enc)
	}
}
