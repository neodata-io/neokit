// Package tracing wires OpenTelemetry distributed tracing for the HTTP server.
//
// It is opt-in: Init is a no-op unless an OTLP endpoint is configured. When
// enabled it exports spans to an OTLP/HTTP collector (Tempo) and installs the
// W3C Trace Context propagator so a `traceparent` header from a trusted upstream
// proxy continues the same trace. The per-request Middleware opens one server span;
// logx then stamps that span's trace_id/span_id onto every log line, which is
// what lets Grafana jump Loki ⇄ Tempo ("logs to trace" and back).
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/logx"
)

// The tracer is resolved from the global provider on each request, so the
// Middleware follows whichever provider is installed, by Init or otherwise.
const scopeName = "github.com/neodata-io/neokit/tracing"

// Config is the application-supplied identity for the traced service. Everything
// else — endpoint, sampler, headers, protocol — comes from the standard OTEL_*
// env vars read by the SDK, so this package adds no tracing knobs of its own.
type Config struct {
	ServiceName string
	Version     string

	// Log receives the enabled/disabled lines. Nil means slog.Default().
	Log *slog.Logger
}

// Init installs the W3C propagator and, when an OTLP endpoint is configured via
// OTEL_EXPORTER_OTLP_ENDPOINT (or the traces-specific override), an OTLP
// exporter + tracer provider. The returned shutdown flushes and stops the
// exporter; it is safe to call even when tracing is disabled. Enabling tracing
// never fails startup: an exporter error is logged and tracing stays off.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	// Always propagate: even with no local exporter a trusted proxy's traceparent
	// is honoured, so trace_id still flows through logs behind a proxy that
	// starts the trace.
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
		log.Warn("tracing disabled: OTLP exporter init failed", logx.Err(err))
		return func(context.Context) error { return nil }, nil
	}

	// Include the standard detectors so the resource carries what Tempo/Grafana
	// expect: WithFromEnv honors OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES (the
	// env-driven ethos this package otherwise relies on), WithTelemetrySDK stamps
	// telemetry.sdk.*. WithAttributes is last so the application-supplied service
	// name wins over any env value.
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
	log.Info("tracing enabled", "endpoint", endpoint)

	return tp.Shutdown, nil
}

// pristine is the global provider before anyone installs one. Spans started on
// it are discarded, so [Middleware] skips the work of making them.
var pristine = otel.GetTracerProvider()

// off reports whether spans would go nowhere.
func off() bool {
	tp := otel.GetTracerProvider()
	if _, ok := tp.(noop.TracerProvider); ok {
		return true
	}
	return tp == pristine
}

// upstream extracts the trace context a trusted proxy sent, if any. A client's
// own traceparent is not honoured: it picks the trace id and the sampling
// decision, so it could force sampling, hide requests, or forge the id in logs.
func upstream(c fiber.Ctx) (context.Context, bool) {
	// IsProxyTrusted is false unless Config.TrustProxy is set and the peer matches.
	if !c.IsProxyTrusted() {
		return nil, false
	}
	var carrier propagation.MapCarrier
	for _, k := range propagated {
		if v := c.Get(k); v != "" {
			if carrier == nil {
				carrier = make(propagation.MapCarrier, len(propagated))
			}
			// Cloned: Fiber's string aliases a pooled buffer, and the parsed
			// tracestate and baggage keep substrings of it.
			carrier[k] = strings.Clone(v)
		}
	}
	if carrier == nil {
		return nil, false
	}
	return otel.GetTextMapPropagator().Extract(c.Context(), carrier), true
}

var propagated = [...]string{"traceparent", "tracestate", "baggage"}

// Middleware opens one server span per request, continuing an upstream trace
// when a trusted proxy (Fiber's TrustProxy configuration) sent one. The span
// context is placed on the request context so downstream handlers, services,
// and every log line share the same trace_id. Register it after requestid and
// before the metrics/logger middleware so the summary line carries the trace ids.
//
// With no tracer provider installed it only forwards a trusted proxy's trace
// context to the logs, and otherwise costs nothing.
func Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		parent, continued := upstream(c)
		if off() {
			if continued {
				c.SetContext(parent)
			}
			return c.Next()
		}
		if !continued {
			parent = c.Context()
		}

		method := c.Method()
		// Named by method alone for now: the route is not matched until c.Next().
		ctx, span := otel.Tracer(scopeName).Start(parent, method,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", method),
				// Cloned: the span is exported after Fiber has reused this buffer.
				attribute.String("url.path", strings.Clone(c.Path())),
			),
		)
		// One deferred function, so the span is named and marked before it ends
		// whether the handler returned or panicked.
		defer func() {
			// Low-cardinality name: METHOD + route template, e.g. "GET
			// /api/invites/:id" — the same label the metrics and the log line use.
			route := c.Route().Path
			span.SetName(method + " " + route)
			span.SetAttributes(attribute.String("http.route", route))

			// A handler panic unwinds through here before the outer recover
			// middleware; mark the span and re-panic so that still responds.
			if r := recover(); r != nil {
				span.SetStatus(codes.Error, "panic")
				span.RecordError(fmt.Errorf("%v", r))
				span.End()
				panic(r)
			}
			span.End()
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
