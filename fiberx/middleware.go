package fiberx

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"go.opentelemetry.io/otel"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/httpconv"

	"github.com/neodata-io/neokit/logx"
)

// ── HTTP server metrics ─────────────────────────────────────────────────────

// meterName scopes the instruments below. It is the instrumentation scope on
// the exported metrics, which is how a collector tells neokit's HTTP metrics
// apart from an application's own.
const meterName = "github.com/neodata-io/neokit/fiberx"

// httpMetrics is the instrument pair MetricsAndLogger records into.
//
// Both come from httpconv rather than being declared by hand, so their names,
// units and — for the histogram — bucket boundaries are the ones a collector
// already has dashboards for. The boundaries matter more than they look: the
// SDK's default set tops out at 10 000, which is right for a millisecond-valued
// instrument and useless for one measured in seconds, where every real request
// lands in the first bucket.
type httpMetrics struct {
	duration httpconv.ServerRequestDuration
	active   httpconv.ServerActiveRequests
}

// newHTTPMetrics builds the instruments from the global MeterProvider.
//
// Here, and not in a package var: metrics.Init installs the provider, and an
// instrument built before that binds to the global delegating one instead. That
// does back-fill correctly in production — but only for the first provider ever
// installed, so a package var would silently pin every test in this package to
// whichever provider the first one happened to set. Building at middleware
// construction ties them to a moment that is already after Init in app.New.
//
// An error here means a malformed instrument name, which is this file's bug and
// not an operator's; httpconv hands back a working no-op either way, so the
// process keeps serving without metrics rather than failing to boot.
func (e *Errors) newHTTPMetrics() httpMetrics {
	meter := otel.Meter(meterName)

	duration, err := httpconv.NewServerRequestDuration(meter)
	if err != nil {
		e.logger().Warn("http duration metric unavailable", logx.Err(err))
	}
	active, err := httpconv.NewServerActiveRequests(meter)
	if err != nil {
		e.logger().Warn("http in-flight metric unavailable", logx.Err(err))
	}
	return httpMetrics{duration: duration, active: active}
}

// requestMethod maps a method onto its semantic-convention attribute, folding
// anything unrecognised onto _OTHER.
//
// Same reason the route attribute is a template: a client may put any token in
// the method line, and an unbounded attribute value on a time series that is
// never evicted is a memory leak with a one-line trigger.
func requestMethod(method string) httpconv.RequestMethodAttr {
	switch method {
	case fiber.MethodGet:
		return httpconv.RequestMethodGet
	case fiber.MethodHead:
		return httpconv.RequestMethodHead
	case fiber.MethodPost:
		return httpconv.RequestMethodPost
	case fiber.MethodPut:
		return httpconv.RequestMethodPut
	case fiber.MethodPatch:
		return httpconv.RequestMethodPatch
	case fiber.MethodDelete:
		return httpconv.RequestMethodDelete
	case fiber.MethodConnect:
		return httpconv.RequestMethodConnect
	case fiber.MethodOptions:
		return httpconv.RequestMethodOptions
	case fiber.MethodTrace:
		return httpconv.RequestMethodTrace
	default:
		return httpconv.RequestMethodOther
	}
}

// MetricsAndLogger is a single middleware that records the OpenTelemetry HTTP
// server metrics and logs every request through slog. One middleware, one pass,
// no duplication. It is a method on *Errors — not a bare function — because a
// returned error's status must be derived the same way Render/WriteError
// derive it: a handler that returns a bare caller sentinel directly (rather
// than calling e.WriteError itself) relies on the app's ErrorHandler and the
// same DomainMapper to render it, and this middleware runs *before* that
// happens (see the status derivation below). A version with no mapper would
// silently record every such request as a 500.
//
// The instruments are built once, when this returns — see [Errors.newHTTPMetrics].
func (e *Errors) MetricsAndLogger() fiber.Handler {
	m := e.newHTTPMetrics()

	return func(c fiber.Ctx) error {
		start := time.Now()
		method := c.Method()
		reqMethod, scheme := requestMethod(method), c.Scheme()

		// Lift the ID that the requestid middleware (registered just before this
		// one) already generated into the Go context, so every context-aware log
		// emitted while handling this request — downstream handlers, services, and
		// the summary line below — shares one correlation ID.
		if rid := requestid.FromContext(c); rid != "" {
			c.SetContext(logx.WithRequestID(c.Context(), rid))
		}

		// Deferred, not paired inline: a panic in a downstream handler unwinds
		// straight past a plain decrement, permanently corrupting the counter by
		// one per panicking request — and whether that happens depends on where the
		// recover middleware sits relative to this one. Deferred arguments are
		// evaluated now, so the -1 also carries exactly the attributes the +1 did.
		m.active.Add(c.Context(), 1, reqMethod, scheme)
		defer m.active.Add(c.Context(), -1, reqMethod, scheme)

		err := c.Next()

		// The route template (e.g. /api/invites/:id) — and it can only be read
		// HERE, after c.Next(). Read before the chain runs, the router hasn't
		// matched yet and c.Route() reports *this middleware's own* catch-all Use
		// route, whose path is "/", collapsing every series into one.
		//
		// Why a *template* and not the raw URL: the raw path is attacker-controlled,
		// and a time series is never evicted — one port scan would mint a permanent
		// series per probed URL. A request that matches no route at all falls back
		// to this middleware's own "/" route (a Use route always matches), so
		// unrouted junk lands on a single bounded series rather than minting one
		// each. If this middleware ever stops being registered with Use, re-check
		// that: c.Route() falls back to the *raw* path when nothing matched at all,
		// which is exactly the unbounded case.
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

		// Record everything, including health probes. The SDK attaches a trace
		// exemplar by itself when this request carried a sampled span — its default
		// exemplar filter is trace-based — so a slow bucket in Grafana still links
		// straight to the span in Tempo, with no exemplar plumbing here.
		m.duration.Record(c.Context(), duration.Seconds(), reqMethod, scheme,
			semconv.HTTPRoute(path),
			semconv.HTTPResponseStatusCode(status),
		)

		// Logging is for humans, so it is noisier to be quieter: skip the traffic
		// that carries no signal (health probes, 304s) and only escalate the level
		// for real problems.
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
		e.logger().Log(c.Context(), level, "http",
			"method", method,
			"path", path, // template path, matches the http.route attribute
			"status", status,
			// Milliseconds as a float so LogQL can `unwrap duration_ms` directly for
			// latency dashboards without dividing raw nanoseconds in every query.
			"duration_ms", float64(duration.Nanoseconds())/1e6,
		)

		return err
	}
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
