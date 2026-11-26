package tracing

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	recovermw "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	clientTraceID     = "0af7651916cd43dd8448eb211c80319c"
	clientTraceparent = "00-" + clientTraceID + "-b7ad6b7169203331-01"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// record installs a recording provider for one test and puts the previous one
// back afterwards, so the tests that need tracing off still find it off.
func record(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return rec
}

// newServer mounts the middleware the way the app builder does: with Use, ahead
// of the routes. trusted makes the test connection's peer a trusted proxy.
func newServer(trusted bool) *fiber.App {
	cfg := fiber.Config{}
	if trusted {
		cfg.TrustProxy = true
		cfg.TrustProxyConfig = fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0"}}
	}
	f := fiber.New(cfg)
	if _, err := Init(context.Background(), Config{ServiceName: "test", Log: quiet()}); err != nil {
		panic(err)
	}
	f.Use(Middleware())
	return f
}

func get(t *testing.T, f *fiber.App, path string, headers map[string]string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := f.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	_ = resp.Body.Close()
}

func attr(s sdktrace.ReadOnlySpan, key string) string {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.String()
		}
	}
	return ""
}

func noEndpoint(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
}

func TestSpanIsNamedAfterTheMatchedRouteTemplate(t *testing.T) {
	noEndpoint(t)
	rec := record(t)
	f := newServer(false)
	f.Get("/api/invites/:id", func(c fiber.Ctx) error { return c.SendString("ok") })

	get(t, f, "/api/invites/42", nil)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if got, want := spans[0].Name(), "GET /api/invites/:id"; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
	if got, want := attr(spans[0], "http.route"), "/api/invites/:id"; got != want {
		t.Errorf("http.route = %q, want %q", got, want)
	}
	if got := attr(spans[0], "http.response.status_code"); got != "200" {
		t.Errorf("http.response.status_code = %q, want 200", got)
	}
}

// Fiber hands out strings that alias a pooled request buffer. A span outlives
// the request — the batcher exports it seconds later — so anything it keeps
// must be a copy, or it ends up carrying a later request's bytes.
func TestSpanKeepsItsOwnStringsAfterLaterRequests(t *testing.T) {
	noEndpoint(t)
	rec := record(t)
	f := newServer(true)
	f.Get("/api/invites/:id", func(c fiber.Ctx) error { return c.SendString("ok") })

	firstState := "vendor=" + strings.Repeat("a", 40)
	get(t, f, "/api/invites/"+strings.Repeat("A", 40), map[string]string{"traceparent": clientTraceparent, "tracestate": firstState})
	for range 50 {
		get(t, f, "/api/invites/"+strings.Repeat("B", 40), map[string]string{
			"traceparent": clientTraceparent, "tracestate": "vendor=" + strings.Repeat("b", 40),
		})
	}

	first := rec.Ended()[0]
	if got := first.SpanContext().TraceState().String(); got != firstState {
		t.Errorf("tracestate = %q, want %q", got, firstState)
	}
}

func TestTraceparentFromAnUntrustedPeerStartsAFreshTrace(t *testing.T) {
	noEndpoint(t)
	rec := record(t)
	f := newServer(false)
	var baggage string
	f.Get("/x", func(c fiber.Ctx) error { baggage = baggageFrom(c.Context()); return nil })

	get(t, f, "/x", map[string]string{"traceparent": clientTraceparent, "tracestate": "vendor=x", "baggage": "k=v"})

	span := rec.Ended()[0]
	if got := span.SpanContext().TraceID().String(); got == clientTraceID {
		t.Error("the span continued a trace id chosen by an untrusted client")
	}
	if span.Parent().IsValid() {
		t.Errorf("span has parent %v, want a root span", span.Parent().SpanID())
	}
	if got := span.SpanContext().TraceState().String(); got != "" {
		t.Errorf("tracestate = %q, want none from an untrusted client", got)
	}
	if baggage != "" {
		t.Errorf("baggage %q from an untrusted client reached the request context", baggage)
	}
}

func TestTraceparentFromATrustedProxyIsContinued(t *testing.T) {
	noEndpoint(t)
	rec := record(t)
	f := newServer(true)
	var baggage string
	f.Get("/x", func(c fiber.Ctx) error { baggage = baggageFrom(c.Context()); return nil })

	get(t, f, "/x", map[string]string{"traceparent": clientTraceparent, "baggage": "k=v"})

	span := rec.Ended()[0]
	if got := span.SpanContext().TraceID().String(); got != clientTraceID {
		t.Errorf("trace id = %s, want the proxy's %s", got, clientTraceID)
	}
	if !span.Parent().IsRemote() {
		t.Error("span parent is not the remote span the proxy named")
	}
	if baggage != "k=v" {
		t.Errorf("baggage = %q, want k=v", baggage)
	}
}

// With no provider installed the middleware still carries a trusted proxy's
// trace id to the logs, and still refuses a client's.
func TestWithTracingOffOnlyATrustedTraceparentReachesTheContext(t *testing.T) {
	noEndpoint(t)
	for _, tc := range []struct {
		name    string
		trusted bool
		want    string
	}{
		{"trusted proxy", true, clientTraceID},
		{"untrusted client", false, "00000000000000000000000000000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServer(tc.trusted)
			var got string
			f.Get("/x", func(c fiber.Ctx) error {
				got = trace.SpanContextFromContext(c.Context()).TraceID().String()
				return nil
			})
			get(t, f, "/x", map[string]string{"traceparent": clientTraceparent})
			if got != tc.want {
				t.Errorf("trace id on the request context = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPanickingHandlerMarksTheSpanAndStillPanics(t *testing.T) {
	noEndpoint(t)
	rec := record(t)
	f := fiber.New()
	f.Use(recovermw.New())
	f.Use(Middleware())
	f.Get("/boom/:id", func(fiber.Ctx) error { panic("kaboom") })

	req := httptest.NewRequest("GET", "/boom/1", nil)
	resp, err := f.Test(req)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want the recover middleware's 500", resp.StatusCode)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", spans[0].Status().Code)
	}
	if got, want := spans[0].Name(), "GET /boom/:id"; got != want {
		t.Errorf("span name = %q, want %q", got, want)
	}
	if len(spans[0].Events()) == 0 {
		t.Error("the panic was not recorded on the span")
	}
}

func TestServerErrorStatusMarksTheSpan(t *testing.T) {
	noEndpoint(t)
	rec := record(t)
	f := newServer(false)
	f.Get("/down", func(c fiber.Ctx) error { return c.SendStatus(503) })

	get(t, f, "/down", nil)

	if got := rec.Ended()[0].Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error for a 503", got)
	}
}

func TestInitWithoutAnEndpointInstallsNoProvider(t *testing.T) {
	noEndpoint(t)
	before := otel.GetTracerProvider()

	shutdown, err := Init(context.Background(), Config{ServiceName: "svc", Log: quiet()})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if otel.GetTracerProvider() != before {
		t.Error("Init installed a tracer provider with no endpoint configured")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown of disabled tracing = %v, want nil", err)
	}
}

func TestInitWithAnEndpointInstallsARecordingProvider(t *testing.T) {
	noEndpoint(t)
	// Nothing listens here; the exporter only dials when it has spans to send.
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/v1/traces")
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	shutdown, err := Init(context.Background(), Config{ServiceName: "svc", Version: "1.2.3", Log: quiet()})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, span := otel.Tracer("test").Start(context.Background(), "probe")
	if !span.IsRecording() {
		t.Error("span is not recording after Init with an endpoint")
	}
	// Left unended, so shutdown has nothing to flush at a dead endpoint.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown = %v", err)
	}
}

func baggageFrom(ctx context.Context) string { return baggage.FromContext(ctx).String() }

// The raw path can carry a token (an invite, a reset link); a span names the
// route template only.
func TestSpanDoesNotCarryTheRawPath(t *testing.T) {
	noEndpoint(t)
	rec := record(t)
	f := newServer(false)
	f.Get("/api/invites/:id", func(c fiber.Ctx) error { return c.SendString("ok") })

	get(t, f, "/api/invites/SECRETTOKEN123", nil)

	for _, a := range rec.Ended()[0].Attributes() {
		if strings.Contains(a.Value.String(), "SECRETTOKEN123") {
			t.Errorf("attribute %s = %q carries the raw path", a.Key, a.Value.String())
		}
	}
}

// An OTLP endpoint may carry credentials in its userinfo or a secret path; the
// startup line names the host.
func TestInitLogsOnlyTheEndpointHost(t *testing.T) {
	noEndpoint(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://user:pw@127.0.0.1:1/secret/v1/traces")
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))

	shutdown, err := Init(context.Background(), Config{ServiceName: "svc", Log: log})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	if out := logged.String(); strings.Contains(out, "pw") || strings.Contains(out, "secret") || !strings.Contains(out, "127.0.0.1:1") {
		t.Errorf("startup line = %q, want only the host", out)
	}
}

// benchHandler drives one GET through f without the cost of f.Test's
// connection plumbing, so the numbers are the middleware's own.
func benchHandler(b *testing.B, f *fiber.App) {
	b.Helper()
	f.Get("/api/invites/:id", func(fiber.Ctx) error { return nil })
	h := f.Handler()
	var rc fasthttp.RequestCtx
	rc.Request.Header.SetMethod("GET")
	rc.Request.SetRequestURI("/api/invites/42")
	b.ReportAllocs()
	for b.Loop() {
		h(&rc)
	}
}

// The floor: the same request with no tracing middleware at all.
func BenchmarkBareFiber(b *testing.B) { benchHandler(b, fiber.New()) }

// The common deployment: tracing mounted, no collector configured.
func BenchmarkMiddleware_TracingOff(b *testing.B) {
	f := fiber.New()
	f.Use(Middleware())
	benchHandler(b, f)
}

func BenchmarkMiddleware_Recording(b *testing.B) {
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	b.Cleanup(func() { otel.SetTracerProvider(prev) })
	f := fiber.New()
	f.Use(Middleware())
	benchHandler(b, f)
}
