package fiberx

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/trace"

	"github.com/neodata-io/neokit/logx"
)

// ── Prometheus metrics ──────────────────────────────────────────────────────

var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests by method, path, and status.",
	}, []string{"method", "path", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	httpRequestsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "http_requests_in_flight",
		Help: "Number of HTTP requests currently being handled.",
	})
)

// MetricsAndLogger is a single middleware that records Prometheus metrics
// and logs every request through slog. One middleware, one pass, no
// duplication. It is a method on *Errors — not a bare function — because a
// returned error's status must be derived the same way Render/WriteError
// derive it: a handler that returns a bare caller sentinel directly (rather
// than calling e.WriteError itself) relies on the app's ErrorHandler and the
// same DomainMapper to render it, and this middleware runs *before* that
// happens (see the status derivation below). A version with no mapper would
// silently record every such request as a 500.
func (e *Errors) MetricsAndLogger() fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()
		method := c.Method()

		// Lift the ID that the requestid middleware (registered just before this
		// one) already generated into the Go context, so every context-aware log
		// emitted while handling this request — downstream handlers, services, and
		// the summary line below — shares one correlation ID.
		if rid := requestid.FromContext(c); rid != "" {
			c.SetContext(logx.WithRequestID(c.Context(), rid))
		}

		// Deferred, not paired inline: a panic in a downstream handler unwinds
		// straight past a plain Dec(), permanently corrupting the gauge by one per
		// panicking request — and whether that happens depends on where the recover
		// middleware sits relative to this one.
		httpRequestsInFlight.Inc()
		defer httpRequestsInFlight.Dec()

		err := c.Next()

		// The route template (e.g. /api/invites/:id) — and it can only be read
		// HERE, after c.Next(). Read before the chain runs, the router hasn't
		// matched yet and c.Route() reports *this middleware's own* catch-all Use
		// route, whose path is "/", collapsing every series into one.
		//
		// Why a *template* and not the raw URL: the raw path is attacker-controlled,
		// and a Prometheus label is never evicted — one port scan would mint a
		// permanent series per probed URL. A request that matches no route at all
		// falls back to this middleware's own "/" route (a Use route always
		// matches), so unrouted junk lands on a single bounded series rather than
		// minting one each. If this middleware ever stops being registered with
		// Use, re-check that: c.Route() falls back to the *raw* path when nothing
		// matched at all, which is exactly the unbounded case.
		path := c.Route().Path

		// A returned error hasn't been rendered yet — the global ErrorHandler runs
		// after this middleware unwinds, so c.Response().StatusCode() is still the
		// default 200. Derive the real status from the error (a returned
		// *fiber.Error from BindAndValidate, or a mapped caller error) so metrics
		// and logs don't record every validation/4xx/5xx failure as a 200.
		status := c.Response().StatusCode()
		if err != nil {
			status = e.StatusForError(err)
		}
		duration := time.Since(start)
		statusStr := strconv.Itoa(status)

		// Prometheus — record everything, including scrapes and health checks. The
		// duration histogram carries a trace exemplar when this request was sampled,
		// so a slow bucket in Grafana links straight to the span in Tempo.
		httpRequestsTotal.WithLabelValues(method, path, statusStr).Inc()
		observeDuration(c.Context(), method, path, duration.Seconds())

		// Logging is for humans, so it is noisier to be quieter: skip the traffic
		// that carries no signal (metric scrapes, health probes, 304s) and only
		// escalate the level for real problems.
		if e.skipLog(path, status) {
			return err
		}

		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		// requestId is stamped on automatically by logx.ContextHandler from the
		// context set above — no need to add it here.
		slog.Log(c.Context(), level, "http",
			"method", method,
			"path", path, // template path, matches the Prometheus label
			"status", status,
			// Milliseconds as a float so LogQL can `unwrap duration_ms` directly for
			// latency dashboards without dividing raw nanoseconds in every query.
			"duration_ms", float64(duration.Nanoseconds())/1e6,
		)

		return err
	}
}

// observeDuration records the request duration, attaching a trace_id exemplar when
// the request carried a sampled span. Exemplars are the seam that turns "this p99
// bucket is slow" into a click through to the exact trace: Grafana maps the
// histogram's exemplar trace_id to Tempo (the span context is on the request ctx
// because tracing.Middleware runs before this one). Only sampled spans get one —
// exemplars are meant to be sparse pointers at recorded traces — and only when the
// concrete observer supports them, which the classic Prometheus histogram does. A
// disabled tracer means an empty span context, so this silently falls back to a
// plain Observe.
func observeDuration(ctx context.Context, method, path string, secs float64) {
	obs := httpRequestDuration.WithLabelValues(method, path)
	if sc := trace.SpanContextFromContext(ctx); sc.IsSampled() {
		if eo, ok := obs.(prometheus.ExemplarObserver); ok {
			eo.ObserveWithExemplar(secs, prometheus.Labels{"trace_id": sc.TraceID().String()})
			return
		}
	}
	obs.Observe(secs)
}

// skipLog reports whether a request is pure noise that would drown the useful
// lines: not-modified/switching-protocols responses (browser caching, SSE
// upgrades), plus whatever e.QuietPath calls out. Anything 4xx/5xx is always
// logged so failures on these paths still surface.
func (e *Errors) skipLog(path string, status int) bool {
	if status >= 400 {
		return false
	}
	if status == 304 || status == 101 {
		return true
	}
	return e.QuietPath != nil && e.QuietPath(path)
}
