// Package tracing wires OpenTelemetry distributed tracing for the HTTP server.
//
// It is opt-in: Init is a no-op unless an OTLP endpoint is configured. When
// enabled it exports spans to an OTLP/HTTP collector (Tempo) and installs the
// W3C Trace Context propagator so a `traceparent` header from an upstream proxy
// continues the same trace. The per-request Middleware opens one server span;
// logx then stamps that span's trace_id/span_id onto every log line, which is
// what lets Grafana jump Loki ⇄ Tempo ("logs to trace" and back).
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/logx"
)

// tracer is resolved lazily from the global provider so the Middleware works
// whether or not Init installed a real provider (no-op provider otherwise).
const scopeName = "github.com/neodata-io/neokit/tracing"

// Config is the host-supplied identity for the traced service. Everything else
// — endpoint, sampler, headers, protocol — comes from the standard OTEL_* env
// vars read by the SDK, so there are no NeoGate-specific tracing knobs.
type Config struct {
	ServiceName string
	Version     string
}

// Init installs the W3C propagator and, when an OTLP endpoint is configured via
// OTEL_EXPORTER_OTLP_ENDPOINT (or the traces-specific override), an OTLP
// exporter + tracer provider. The returned shutdown flushes and stops the
// exporter; it is safe to call even when tracing is disabled. Enabling tracing
// never fails startup: an exporter error is logged and tracing stays off.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	// Always propagate: even with no local exporter we honor and forward an
	// upstream traceparent, so trace_id still flows through logs behind a proxy
	// that starts the trace.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Enablement gate: the same standard env var the SDK's exporter reads.
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	}
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	// OTLP/HTTP only. The exporter reads the endpoint and the other transport
	// knobs it supports (headers, TLS) from the environment; we pass nothing so
	// standard OTel configuration works unchanged. Note OTEL_EXPORTER_OTLP_PROTOCOL
	// is not honored — the transport is fixed to HTTP by construction here.
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		slog.Warn("tracing disabled: OTLP exporter init failed", logx.Err(err))
		return func(context.Context) error { return nil }, nil
	}

	// Include the standard detectors so the resource carries what Tempo/Grafana
	// expect: WithFromEnv honors OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES (the
	// env-driven ethos this package otherwise relies on), WithTelemetrySDK stamps
	// telemetry.sdk.*. WithAttributes is last so the host-supplied service name
	// wins over any env value.
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
		),
	)
	if err != nil {
		// A partial resource (e.g. schema-URL conflict) is still usable.
		res = resource.NewSchemaless(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
		)
	}

	// No WithSampler: the SDK's default sampler honors OTEL_TRACES_SAMPLER /
	// OTEL_TRACES_SAMPLER_ARG (default parentbased_always_on), so sampling is
	// tuned by standard env vars rather than a bespoke knob.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	slog.Info("tracing enabled", "endpoint", endpoint)

	return tp.Shutdown, nil
}

// Middleware opens one server span per request, continuing an upstream trace
// when present. The span context is placed on the request context so downstream
// handlers, services, and every log line share the same trace_id. Register it
// after requestid and before the metrics/logger middleware so the summary line
// carries the trace ids.
func Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		// Continue an upstream trace if the proxy/client sent one.
		carrier := propagation.MapCarrier{}
		for _, k := range []string{"traceparent", "tracestate", "baggage"} {
			if v := c.Get(k); v != "" {
				carrier[k] = v
			}
		}
		ctx := otel.GetTextMapPropagator().Extract(c.Context(), carrier)

		// Low-cardinality span name: METHOD + route template, e.g. "GET
		// /api/invites/:id" — matches the Prometheus/log path label.
		route := c.Route().Path
		ctx, span := otel.Tracer(scopeName).Start(ctx, c.Method()+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Method()),
				attribute.String("http.route", route),
				attribute.String("url.path", c.Path()),
			),
		)
		// A handler panic unwinds through here before the outer recover
		// middleware catches it; without this the span would end Unset with no
		// status_code — a blind spot on exactly the worst requests. Mark it and
		// re-panic so recover.New still produces the response.
		// Register span.End() FIRST so LIFO runs it LAST — after the recover handler
		// below has marked the span errored. If End ran first (as when it was
		// registered last), the recover handler's SetStatus/RecordError would no-op
		// on an already-ended span, leaving a panicking request as an Unset span with
		// no recorded error — the exact blind spot this instrumentation exists to
		// close. End still runs as the re-panic unwinds.
		defer span.End()
		defer func() {
			if r := recover(); r != nil {
				span.SetStatus(codes.Error, "panic")
				span.RecordError(fmt.Errorf("%v", r))
				panic(r)
			}
		}()

		c.SetContext(ctx)
		err := c.Next()

		status := c.Response().StatusCode()
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= 500 {
			span.SetStatus(codes.Error, "")
		}
		if err != nil {
			span.RecordError(err)
		}
		return err
	}
}
