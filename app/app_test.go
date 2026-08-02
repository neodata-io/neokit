package app_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newApp(t *testing.T) *app.App {
	t.Helper()
	a, err := app.New(app.Options{
		Name: "testapp", Version: "1.2.3",
		Base: config.Base{Port: 0, LogLevel: "error", LogFormat: "json"},
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

// Version is the opposite call: it defaults rather than demanding, because Go
// already knows the answer. An empty one must never reach the boot report, the
// logs or the metrics resource as a blank field.
func TestVersionDefaultsRatherThanBeingBlank(t *testing.T) {
	a, err := app.New(app.Options{Name: "testapp", Base: config.Base{Port: 0}, Log: quiet()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if a.Version == "" {
		t.Error("Version is blank — an unset one must fall back to the embedded build identity")
	}
	// "dev" is buildinfo's marker for an unstamped build. Asserting the marker
	// rather than a commit keeps this passing under `go run`, which embeds no VCS
	// metadata at all.
	if !strings.Contains(a.Version, "dev") {
		t.Errorf("Version = %q, want the dev marker for an unstamped build", a.Version)
	}
	if !strings.Contains(a.Report(), a.Version) {
		t.Errorf("the boot report does not carry the version:\n%s", a.Report())
	}
}

// A version the caller supplied is theirs — the fallback fills a blank, it never
// edits what it was given.
func TestASuppliedVersionIsUsedVerbatim(t *testing.T) {
	a, err := app.New(app.Options{
		Name: "testapp", Version: "1.4.0", Base: config.Base{Port: 0}, Log: quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	if a.Version != "1.4.0" {
		t.Errorf("Version = %q, want the caller's 1.4.0 untouched", a.Version)
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
	for _, name := range []string{"tracing", "metrics export", "health"} {
		if _, ok := findSubsystem(a, name); !ok {
			t.Errorf("%q missing from Subsystems(): %+v", name, a.Subsystems())
		}
	}
}

// The probes now sit on the application listener, so where they are is the one
// thing about them an operator cannot infer from anywhere else — and it decides
// whether they are reachable by anyone who can reach the API. The report has to
// name them.
func TestTheReportNamesTheProbeEndpoints(t *testing.T) {
	a := newApp(t)

	got, ok := findSubsystem(a, "health")
	if !ok {
		t.Fatalf("no health line: %+v", a.Subsystems())
	}
	for _, path := range []string{app.LivePath, app.ReadyPath} {
		if !strings.Contains(got.Detail, path) {
			t.Errorf("detail = %q, want it to name %q", got.Detail, path)
		}
		if !strings.Contains(a.Report(), path) {
			t.Errorf("boot report does not name %q:\n%s", path, a.Report())
		}
	}
}

// Liveness must answer without touching a dependency: a probe that consults the
// database gets a healthy container killed during a database blip.
func TestLivenessAnswersWithoutAnyCheck(t *testing.T) {
	a := newApp(t)
	a.Declare(app.Subsystem{
		Name: "database", On: true, Detail: "down",
		Ready: func(context.Context) error { return errors.New("unreachable") },
	})

	resp, err := a.HTTP.Test(httptest.NewRequest(http.MethodGet, app.LivePath, nil),
		fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — liveness must not consult a check", resp.StatusCode)
	}
}

// Readiness does run them, so a failing dependency takes this instance out of
// rotation without restarting it.
func TestReadinessReflectsADeclaredCheck(t *testing.T) {
	a := newApp(t)
	a.Declare(app.Subsystem{
		Name: "database", On: true, Detail: "down",
		Ready: func(context.Context) error { return errors.New("unreachable") },
	})

	resp, err := a.HTTP.Test(httptest.NewRequest(http.MethodGet, app.ReadyPath, nil),
		fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("status = 200, want a failing check to make the app unready")
	}
}

// The two ends of the split, asserted together so neither can drift: the public
// probe gives away nothing about what this process depends on, and ReadyDetail
// gives an authenticated caller all of it.
func TestReadinessDetailIsSeparateFromThePublicProbe(t *testing.T) {
	a := newApp(t)
	a.Declare(app.Subsystem{
		Name: "database", On: true, Detail: "down",
		Ready: func(context.Context) error { return errors.New("connection refused") },
	})

	_, public := get(t, a, app.ReadyPath, "")
	if strings.Contains(public, "database") || strings.Contains(public, "connection refused") {
		t.Errorf("the public probe leaks a dependency and its error: %s", public)
	}

	// Mounted the way a caller would, except that the guard is what they add.
	a.HTTP.Get("/admin/readyz", adaptor.HTTPHandler(a.ReadyDetail()))

	status, detail := get(t, a, "/admin/readyz", "")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 — the two must not disagree about the verdict", status)
	}
	if !strings.Contains(detail, "database") || !strings.Contains(detail, "connection refused") {
		t.Errorf("the detailed handler is missing the failing check: %s", detail)
	}
}

// The probes are registered above the middleware chain on purpose: probe traffic
// is the highest-volume, lowest-information a service sees, and counting it in
// the request histogram drags every latency percentile toward the cost of
// answering {"status":"ok"}. Registration order is what keeps it out.
func TestProbesBypassTheRequestLog(t *testing.T) {
	var logged bytes.Buffer
	a, err := app.New(app.Options{
		Name: "testapp",
		Base: config.Base{LogLevel: "debug", LogFormat: "json"},
		Log:  slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	a.HTTP.Get("/ordinary", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	for _, path := range []string{app.LivePath, app.ReadyPath, "/ordinary"} {
		resp, err := a.HTTP.Test(httptest.NewRequest(http.MethodGet, path, nil),
			fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("Test %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}

	for _, path := range []string{app.LivePath, app.ReadyPath} {
		if strings.Contains(logged.String(), path) {
			t.Errorf("the %s probe reached the request log:\n%s", path, logged.String())
		}
	}
	// The control: without it this passes just as well when the logger is broken,
	// which is the failure mode that would hide a real regression here.
	if !strings.Contains(logged.String(), "/ordinary") {
		t.Errorf("ordinary traffic was not logged, so the assertions above prove nothing:\n%s", logged.String())
	}
}

// get issues a GET against the app, optionally with an Authorization header.
func get(t *testing.T, a *app.App, path, authorization string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := a.HTTP.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// The endpoint is on by default, and it has to carry the instruments this
// process actually declared — a page that answers 200 with nothing on it looks
// exactly like a working one until someone tries to graph it.
func TestMetricsEndpointServesTheProcessInstruments(t *testing.T) {
	a := newApp(t)
	a.HTTP.Get("/ordinary", func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })

	if status, _ := get(t, a, "/ordinary", ""); status != http.StatusOK {
		t.Fatalf("warm-up request: status = %d, want 200", status)
	}

	status, body := get(t, a, app.MetricsPath, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the endpoint is on unless MetricsDisabled", status)
	}
	// OTel's name translation is what makes the OTLP-native instrument readable
	// to a scraper: http.server.request.duration + unit "s" becomes this.
	if !strings.Contains(body, "http_server_request_duration_seconds") {
		t.Errorf("no HTTP histogram in the exposition:\n%s", body)
	}
	if !strings.Contains(body, "/ordinary") {
		t.Errorf("the route just served is missing from the exposition:\n%s", body)
	}
}

// A token turns the endpoint from public to bearer-only. The wrong token and the
// missing header must both fail, or the guard is decorative.
func TestMetricsTokenIsEnforced(t *testing.T) {
	const token = "s3cr3t"
	a, err := app.New(app.Options{
		Name: "testapp",
		Base: config.Base{Port: 0, MetricsToken: token, LogLevel: "error", LogFormat: "json"},
		Log:  quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	for _, tc := range []struct {
		name, authorization string
		want                int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"right token, wrong scheme", "Basic " + token, http.StatusUnauthorized},
		// A prefix of the real token: the case a byte-at-a-time comparison would
		// leak the length of through timing, and must simply be rejected.
		{"prefix of the token", "Bearer s3c", http.StatusUnauthorized},
		{"correct", "Bearer " + token, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, _ := get(t, a, app.MetricsPath, tc.authorization); status != tc.want {
				t.Errorf("status = %d, want %d", status, tc.want)
			}
		})
	}

	got, _ := findSubsystem(a, "metrics endpoint")
	if !strings.Contains(got.Detail, "bearer token required") {
		t.Errorf("report detail = %q, want it to say the endpoint is authenticated", got.Detail)
	}
}

// The report must say when the endpoint is public, because nothing else will.
// An unset METRICS_TOKEN is silent everywhere else: the endpoint answers either
// way, and the difference only shows up when someone else reads it.
func TestTheReportSaysWhenMetricsAreUnauthenticated(t *testing.T) {
	a := newApp(t)

	got, ok := findSubsystem(a, "metrics endpoint")
	if !ok {
		t.Fatalf("no metrics endpoint line: %+v", a.Subsystems())
	}
	if !strings.Contains(got.Detail, "unauthenticated") || !strings.Contains(got.Detail, "METRICS_TOKEN") {
		t.Errorf("detail = %q, want it to name the exposure and the setting that closes it", got.Detail)
	}
}

// A scrape must not measure itself. At a 15-second interval, an endpoint that
// records a data point about being read is a visible fraction of the traffic on
// an idle service, and it inflates its own request count forever.
func TestScrapesAreNotThemselvesMeasuredOrLogged(t *testing.T) {
	var logged bytes.Buffer
	a, err := app.New(app.Options{
		Name: "testapp",
		Base: config.Base{Port: 0, LogLevel: "debug", LogFormat: "json"},
		Log:  slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	for range 2 {
		if status, _ := get(t, a, app.MetricsPath, ""); status != http.StatusOK {
			t.Fatalf("scrape: status = %d, want 200", status)
		}
	}

	// The second scrape would report the first if the endpoint were behind the
	// middleware, so its own path must not appear in its own output.
	_, body := get(t, a, app.MetricsPath, "")
	if strings.Contains(body, app.MetricsPath) {
		t.Errorf("the endpoint measured its own scrapes:\n%s", body)
	}
	if strings.Contains(logged.String(), app.MetricsPath) {
		t.Errorf("a scrape reached the request log:\n%s", logged.String())
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
