package logx

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// discardHandler is a minimal handler that does nothing, so these benchmarks
// measure logx's own overhead rather than the cost of formatting and writing.
type discardHandler struct{ level slog.Level }

func (h discardHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h discardHandler) Handle(context.Context, slog.Record) error    { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler           { return h }
func (h discardHandler) WithGroup(string) slog.Handler                { return h }

// benchRecord builds a fresh record per iteration the way slog itself does.
func benchRecord() slog.Record {
	return slog.NewRecord(testTime, slog.LevelInfo, "a message", 0)
}

// testTime is a fixed instant so no benchmark pays for time.Now.
var testTime = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

// BenchmarkErr measures the canonical error attribute — the single most-called
// symbol in the library (95 call sites in the reference consumer).
func BenchmarkErr(b *testing.B) {
	err := io.EOF
	b.ReportAllocs()
	for b.Loop() {
		sink = Err(err)
	}
}

var sink slog.Attr

// BenchmarkContextHandler_Handle_Bare measures the per-log-line overhead when
// the context carries neither a request ID nor a span — the background/startup
// case, and the floor this decorator can possibly cost.
func BenchmarkContextHandler_Handle_Bare(b *testing.B) {
	h := NewContextHandler(discardHandler{})
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_ = h.Handle(ctx, benchRecord())
	}
}

// BenchmarkContextHandler_Handle_RequestID measures the common request-scoped
// case: an ID on the context, no active span.
func BenchmarkContextHandler_Handle_RequestID(b *testing.B) {
	h := NewContextHandler(discardHandler{})
	ctx := WithRequestID(context.Background(), "01J0000000000000000000")
	b.ReportAllocs()
	for b.Loop() {
		_ = h.Handle(ctx, benchRecord())
	}
}

// BenchmarkContextHandler_Handle_Traced measures a log line emitted inside a
// sampled span — the fully-loaded path, where the trace and span IDs are
// rendered onto every record.
func BenchmarkContextHandler_Handle_Traced(b *testing.B) {
	h := NewContextHandler(discardHandler{})
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(
		WithRequestID(context.Background(), "01J0000000000000000000"), sc)
	b.ReportAllocs()
	for b.Loop() {
		_ = h.Handle(ctx, benchRecord())
	}
}

// BenchmarkContextHandler_Disabled proves the decorator costs nothing at all
// for a level that is filtered out. A logger must be able to carry debug calls
// in production without paying for them.
func BenchmarkContextHandler_Disabled(b *testing.B) {
	log := slog.New(NewContextHandler(discardHandler{level: slog.LevelError}))
	ctx := WithRequestID(context.Background(), "01J0000000000000000000")
	err := io.EOF
	b.ReportAllocs()
	for b.Loop() {
		log.DebugContext(ctx, "not emitted", Err(err))
	}
}

// BenchmarkRequestID measures the context lookup on its own.
func BenchmarkRequestID(b *testing.B) {
	ctx := WithRequestID(context.Background(), "01J0000000000000000000")
	b.ReportAllocs()
	for b.Loop() {
		strSink = RequestID(ctx)
	}
}

var strSink string

// BenchmarkEndToEnd_Info is the realistic shape: a context-aware call at an
// enabled level with an error attached, through the real JSON handler.
func BenchmarkEndToEnd_Info(b *testing.B) {
	log := slog.New(NewContextHandler(slog.NewJSONHandler(io.Discard, nil)))
	ctx := WithRequestID(context.Background(), "01J0000000000000000000")
	err := io.EOF
	b.ReportAllocs()
	for b.Loop() {
		log.InfoContext(ctx, "handled request", "status", 200, Err(err))
	}
}
