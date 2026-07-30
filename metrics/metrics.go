// Package metrics wires the OpenTelemetry metrics pillar — the sibling of package
// tracing, and the third leg (with tracing plus the trace-correlated logs logx
// already emits) of a full traces + metrics + logs setup.
//
// Like tracing it is opt-in and env-driven: Init is a no-op unless an OTLP endpoint
// is configured, and every knob (endpoint, headers, TLS) comes from the standard
// OTEL_* env vars the SDK reads — there are no NeoGate-specific metrics knobs. When
// enabled it pushes metrics via OTLP/HTTP to a collector and registers Go runtime
// instrumentation (goroutines, GC pauses, heap) so a long-running 29-plugin host is
// observable without hand-instrumenting a thing.
//
// Division of labour with the existing Prometheus endpoint: request-level metrics
// stay on the /metrics pull endpoint (see httpx.MetricsAndLogger), where the HTTP
// duration histogram now carries trace exemplars so a slow bucket in Grafana links
// straight to its span in Tempo. This package adds the *push* pillar and the
// runtime signals. When no endpoint is configured the global MeterProvider stays
// the SDK's no-op, so nothing here costs anything.
package metrics

import (
	"context"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"github.com/neodata-io/neokit/logx"
)

// Config is the host-supplied identity for the metered service — the same shape as
// tracing.Config so the composition root passes one identity to both pillars.
type Config struct {
	ServiceName string
	Version     string
}

// runtimeReadInterval bounds how often the runtime instrumentation re-reads
// MemStats (a stop-the-world call). One second is plenty fresh for GC/heap trends
// and keeps the read cheap on a host that lives for weeks.
const runtimeReadInterval = time.Second

// Init installs an OTLP metric exporter, a periodic-reader MeterProvider, and Go
// runtime instrumentation — but only when an OTLP endpoint is configured via
// OTEL_EXPORTER_OTLP_ENDPOINT (or the metrics-specific override). The returned
// shutdown flushes and stops the reader; it is safe to call even when metrics are
// disabled. Enabling metrics never fails startup: an exporter error is logged and
// metrics stay off, exactly like tracing.Init.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }

	// Enablement gate: the same standard env var the SDK's exporter reads, plus the
	// signal-specific override so metrics can point at a different collector than
	// traces (e.g. traces → Tempo, metrics → Prometheus's OTLP receiver).
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
	}
	if endpoint == "" {
		return noop, nil
	}

	// OTLP/HTTP only. The exporter reads the endpoint and the other transport knobs
	// it supports (headers, TLS) from the environment; we pass nothing so standard
	// OTel configuration works unchanged.
	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		slog.Warn("metrics disabled: OTLP metric exporter init failed", logx.Err(err))
		return noop, nil
	}

	// Same resource construction as tracing.Init, so a span and its host's metrics
	// carry an identical service.name/version and line up in Grafana.
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
		),
	)
	if err != nil {
		res = resource.NewSchemaless(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
		)
	}

	// The PeriodicReader's export interval is the SDK default (60s) unless
	// OTEL_METRIC_EXPORT_INTERVAL is honored by a wrapping config; we keep the
	// default rather than inventing a bespoke knob.
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
	)
	otel.SetMeterProvider(mp)

	// Go runtime metrics (goroutines, GC, heap) on the provider we just installed.
	// A failure here is non-fatal — the pipeline still exports whatever else is
	// instrumented.
	if err := runtime.Start(
		runtime.WithMeterProvider(mp),
		runtime.WithMinimumReadMemStatsInterval(runtimeReadInterval),
	); err != nil {
		slog.Warn("runtime metrics instrumentation failed to start", logx.Err(err))
	}

	slog.Info("metrics enabled", "endpoint", endpoint)
	return mp.Shutdown, nil
}
