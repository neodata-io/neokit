// Package logx holds the small slog helpers that make logs correlatable and
// consistent: a request-ID carried on the context, a handler decorator that
// stamps it onto every record, and a canonical error attribute.
package logx

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type ctxKey int

const requestIDKey ctxKey = iota

// WithRequestID returns a copy of ctx carrying a request correlation ID. The
// HTTP middleware sets this once per request; everything that logs with the
// resulting context (handlers, services, the request-summary line) is then
// stitched together by that ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the correlation ID stored on ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// ContextHandler decorates a slog.Handler so every record automatically carries
// the request-scoped attributes found on the context it was logged with —
// currently the correlation ID. This is what lets a single request's log lines
// share an ID without threading it through every function signature. Only the
// context-aware slog functions (InfoContext, ErrorContext, slog.Log, …) benefit;
// a bare slog.Info with no context simply gets no ID, which is correct for
// startup and background work that runs outside any request.
type ContextHandler struct{ slog.Handler }

// NewContextHandler wraps base so it stamps request-scoped attributes onto every
// record. Wrap the concrete text/JSON handler with this, then SetDefault.
func NewContextHandler(base slog.Handler) slog.Handler {
	return ContextHandler{base}
}

func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("requestId", id))
	}
	// When a trace is active, stamp its ids so Grafana can pivot from a log line
	// to the trace in Tempo and back. Only present when tracing is enabled and
	// the span is sampled; otherwise the span context is invalid and skipped.
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup re-wrap so logger.With(...)-derived loggers keep the
// context behaviour instead of unwrapping back to the bare handler.
func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{h.Handler.WithAttrs(attrs)}
}

func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{h.Handler.WithGroup(name)}
}

// Err is the canonical way to attach an error to a log line. Prefer it over an
// ad-hoc `"error", err` / `"err", err` so the key stays uniform and structured
// error types keep their fields under the JSON handler.
func Err(err error) slog.Attr {
	return slog.Any("error", err)
}
