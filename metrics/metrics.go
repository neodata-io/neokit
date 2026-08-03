// Package metrics wires the OpenTelemetry metrics pillar — the sibling of package
// tracing, and the third leg (with tracing plus the trace-correlated logs logx
// already emits) of a full traces + metrics + logs setup.
//
// # One set of instruments, two ways out
//
// Init installs the global MeterProvider that every instrument in the process
// records into: the HTTP server histogram in fiberx.MetricsAndLogger, the Go
// runtime signals registered here, and whatever the application declares for
// itself. Nothing is written twice and nothing is declared twice — the two exits
// below are readers on that one provider, so a metric added anywhere appears on
// both without being touched again.
//
//   - **Pull.** [Pipeline.Handler] serves the Prometheus text format, which the
//     app builder mounts at /metrics. On by default, because a self-hosted
//     service that cannot be scraped by the Prometheus already running next to it
//     is a service nobody graphs.
//   - **Push.** OTLP/HTTP to a collector, enabled by the standard
//     OTEL_EXPORTER_OTLP_ENDPOINT. Every transport knob (headers, TLS, sampling)
//     comes from the OTEL_* env vars the SDK reads; there are no bespoke ones
//     here. Off unless you have somewhere to push to.
//
// The exemplar filter is trace-based by default, so a histogram observation made
// inside a sampled span carries its trace id out either exit — a slow bucket in
// Grafana links straight to the span in Tempo.
//
// With both off the global provider stays the SDK's no-op and every instrument
// becomes a cheap discard rather than an error, which is what makes it safe to
// instrument code that may run in a process that never called Init.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
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

	// Pull adds the Prometheus reader and populates [Pipeline.Handler]. The app
	// builder always sets it: the endpoint is mounted unconditionally, and
	// METRICS_TOKEN is what guards it.
	Pull bool
}

// Pipeline is what Init installed. The zero value is the disabled one: a nil
// Handler and a Shutdown that does nothing, which is what a caller gets when
// neither exit is configured.
type Pipeline struct {
	// Handler serves the Prometheus text format for every instrument in the
	// process. Nil when Config.Pull was false — a caller mounting it must check,
	// because a nil http.Handler in a route table panics on the first request
	// rather than at boot, which is the worst possible time to find out.
	Handler http.Handler

	// Shutdown flushes and stops the readers. Never nil, so a deferred call needs
	// no guard.
	Shutdown func(context.Context) error
}

// runtimeReadInterval bounds how often the runtime instrumentation re-reads
// MemStats (a stop-the-world call). One second is plenty fresh for GC/heap trends
// and keeps the read cheap on a host that lives for weeks.
const runtimeReadInterval = time.Second

// Init builds the MeterProvider, its readers, and Go runtime instrumentation, and
// installs the provider globally.
//
// Enabling metrics never fails startup. An exporter that cannot be built is
// logged and left out, so a mistyped collector address costs you the push exit,
// not the process — exactly like tracing.Init.
func Init(ctx context.Context, cfg Config) (Pipeline, error) {
	disabled := Pipeline{Shutdown: func(context.Context) error { return nil }}

	var readers []sdkmetric.Reader
	var handler http.Handler

	// Pull first, so a failure to build it still leaves the push exit standing.
	if cfg.Pull {
		// Its own registry rather than prometheus.DefaultRegisterer: that global is
		// where any dependency's init() can register whatever it likes, and this
		// endpoint should publish the instruments this process declared, not
		// whatever happened to be linked into the binary.
		reg := prometheus.NewRegistry()
		exp, err := promexporter.New(promexporter.WithRegisterer(reg))
		if err != nil {
			slog.Warn("metrics endpoint disabled: Prometheus reader init failed", logx.Err(err))
		} else {
			readers = append(readers, exp)
			handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
		}
	}

	// The same standard env var the SDK's exporter reads, plus the signal-specific
	// override so metrics can point at a different collector than traces (e.g.
	// traces → Tempo, metrics → Prometheus's OTLP receiver).
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT")
	}
	if endpoint != "" {
		// OTLP/HTTP only. The exporter reads the endpoint and the other transport
		// knobs it supports (headers, TLS) from the environment; we pass nothing so
		// standard OTel configuration works unchanged.
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			slog.Warn("metrics push disabled: OTLP metric exporter init failed", logx.Err(err))
			endpoint = ""
		} else {
			// The PeriodicReader's export interval is the SDK default (60s); we keep it
			// rather than inventing a bespoke knob.
			readers = append(readers, sdkmetric.NewPeriodicReader(exp))
		}
	}

	// No exit means no provider: the global stays the SDK's no-op, so instruments
	// declared elsewhere cost a nil check instead of accumulating in memory nobody
	// will ever read.
	if len(readers) == 0 {
		return disabled, nil
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

	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
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

	slog.Info("metrics enabled", "pull", handler != nil, "push", endpoint)
	return Pipeline{Handler: handler, Shutdown: mp.Shutdown}, nil
}
