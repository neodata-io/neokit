package metrics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// isolate clears the OTLP env for one test and restores the global provider
// afterwards, so one test's pipeline is not the next one's starting state.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func shutdown(t *testing.T, p Pipeline) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
}

func TestPullHandlerServesEveryInstrumentInTheProcess(t *testing.T) {
	isolate(t)
	p, err := Init(context.Background(), Config{ServiceName: "svc", Version: "1.2.3", Pull: true, Log: quiet()})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer shutdown(t, p)
	if p.Handler == nil {
		t.Fatal("Handler is nil with Pull set")
	}

	// Declared through the global, as application code would, after Init.
	widgets, err := otel.Meter("test").Int64Counter("widgets")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	widgets.Add(context.Background(), 3)

	body := scrape(t, p.Handler)
	for _, want := range []string{
		"widgets_total",           // the application's own instrument
		"go_goroutine_count",      // the runtime instrumentation Init starts
		`service_name="svc"`,      // the resource, on target_info
		`service_version="1.2.3"`, //
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %q", want)
		}
	}
	if !strings.Contains(body, "} 3\n") {
		t.Errorf("scrape does not report the counter's value 3:\n%s", body)
	}
}

// The endpoint publishes what this process declared, not whatever a dependency
// registered on Prometheus's global registry (go_gc_*, process_*, promhttp_*).
func TestPullHandlerLeavesOutTheGlobalPrometheusRegistry(t *testing.T) {
	isolate(t)
	p, err := Init(context.Background(), Config{ServiceName: "svc", Pull: true, Log: quiet()})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer shutdown(t, p)

	body := scrape(t, p.Handler)
	for _, unwanted := range []string{"process_cpu_seconds_total", "promhttp_metric_handler", "go_gc_duration_seconds"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("scrape contains %q from the default registry", unwanted)
		}
	}
}

func TestWithNeitherExitInitInstallsNothing(t *testing.T) {
	isolate(t)
	before := otel.GetMeterProvider()

	p, err := Init(context.Background(), Config{ServiceName: "svc", Log: quiet()})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.Handler != nil {
		t.Error("Handler is set without Pull")
	}
	if otel.GetMeterProvider() != before {
		t.Error("Init installed a meter provider with no reader to serve it")
	}
	if p.Shutdown == nil {
		t.Fatal("Shutdown is nil; a deferred call would panic")
	}
	shutdown(t, p)
}

func TestPushEndpointExportsOnShutdown(t *testing.T) {
	isolate(t)
	var posts atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			posts.Add(1)
		}
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", collector.URL+"/v1/metrics")

	p, err := Init(context.Background(), Config{ServiceName: "svc", Log: quiet()})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.Handler != nil {
		t.Error("Handler is set without Pull")
	}
	shutdown(t, p)

	if posts.Load() == 0 {
		t.Error("Shutdown did not flush the pipeline to the collector")
	}
}
