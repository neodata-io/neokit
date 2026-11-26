package httpc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A span is exported to a collector others read, so it names the upstream host
// and nothing else: bot tokens ride in the path, api keys in the query.
func TestTracedClientSpansCarryOnlyTheHostOfTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	// otelhttp takes its tracer from the span already in the context.
	ctx, parent := tp.Tracer("test").Start(t.Context(), "parent")

	c := NewHTTPClient(HTTPOptions{Tracing: true})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/bot123:SECRET/x?api_key=TOPSECRET", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	drainClose(resp.Body)
	parent.End()

	var client sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() != "parent" {
			client = s
		}
	}
	if client == nil {
		t.Fatal("no client span recorded")
	}
	var sawURL bool
	for _, kv := range client.Attributes() {
		v := kv.Value.Emit()
		if strings.Contains(v, "SECRET") {
			t.Errorf("span attribute %s carries a secret: %q", kv.Key, v)
		}
		if kv.Key == "url.full" {
			sawURL = true
			if v != srv.URL {
				t.Errorf("url.full = %q, want %q", v, srv.URL)
			}
		}
	}
	if !sawURL {
		t.Error("url.full is missing; the span should still name the upstream")
	}
}
