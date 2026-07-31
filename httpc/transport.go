package httpc

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// RetryConfig tunes RetryTransport's backoff. Use DefaultRetryConfig for the
// host's standard policy; a zero MaxRetries disables retries (the request is
// issued exactly once).
type RetryConfig struct {
	// MaxRetries is the number of additional attempts after the first. 0 means
	// no retry.
	MaxRetries int
	// BaseDelay is the wait before the first retry; it doubles each attempt.
	BaseDelay time.Duration
	// MaxDelay caps a single computed backoff (before jitter).
	MaxDelay time.Duration
}

// DefaultRetryConfig is the host's standard policy: two retries (three attempts
// total) with exponential backoff — ~200ms then ~400ms, capped at 2s, plus
// jitter. Enough to ride out a brief blip without hammering a struggling
// upstream.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{MaxRetries: 2, BaseDelay: 200 * time.Millisecond, MaxDelay: 2 * time.Second}
}

// RetryTransport is an http.RoundTripper that retries transient failures —
// network/transport errors, HTTP 429, and 5xx — with exponential backoff and
// jitter. It retries only *idempotent* methods (GET, HEAD, OPTIONS, TRACE), so
// writes (account provisioning, login, player controls) are issued exactly once
// and can never be double-applied by a retry. A server Retry-After header on a
// 429/5xx is honored in place of the computed backoff. Backoff sleeps respect
// the request context, so an overall http.Client.Timeout still bounds the total.
//
// It composes: install it as an *http.Client's Transport, wrapping any base
// RoundTripper (http.DefaultTransport when nil). BaseClient installs it by
// default, so every client that embeds BaseClient is resilient automatically.
type RetryTransport struct {
	base http.RoundTripper
	cfg  RetryConfig
}

// NewRetryTransport wraps base (or http.DefaultTransport when nil) with cfg.
// Pass [DefaultRetryConfig] for the standard policy.
//
// It installs no tracing; use [NewTracedRetryTransport] for that.
func NewRetryTransport(base http.RoundTripper, cfg RetryConfig) *RetryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &RetryTransport{base: base, cfg: cfg}
}

// NewTracedRetryTransport is [NewRetryTransport] plus otel client spans: every
// request opens a span parented to the request's context and carries the
// traceparent header outbound.
//
// otelhttp sits *inside* the retry loop, so each attempt is its own span and a
// retried upstream shows its retries in the trace.
//
// Use it where a collector actually runs. With no provider installed the spans
// go nowhere but still cost ~1.2µs and 22 allocations per request, which is why
// this is a separate constructor rather than the default — see
// BenchmarkTransport_Default and BenchmarkTransport_Traced.
func NewTracedRetryTransport(base http.RoundTripper, cfg RetryConfig) *RetryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	base = otelhttp.NewTransport(base, otelhttp.WithSpanNameFormatter(spanName))
	return &RetryTransport{base: base, cfg: cfg}
}

// spanName gives outbound client spans a low-cardinality, readable name like
// "GET api.vendor.example" — otelhttp's default is the bare method, which
// collapses every upstream into one name in Tempo.
func spanName(_ string, r *http.Request) string {
	return r.Method + " " + r.URL.Host
}

// RoundTrip implements http.RoundTripper.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.cfg.MaxRetries <= 0 || !idempotentMethod(req.Method) {
		return t.base.RoundTrip(req)
	}

	delay := t.cfg.BaseDelay
	var (
		resp *http.Response
		err  error
	)
	for attempt := 0; ; attempt++ {
		// Replaying a request with a body needs a fresh reader. net/http sets
		// GetBody for the standard in-memory body types; without it we can't
		// safely re-send, so return the previous result. (Idempotent requests are
		// almost always bodyless, so this path is rarely reached.)
		if attempt > 0 && req.Body != nil && req.Body != http.NoBody {
			if req.GetBody == nil {
				return resp, err
			}
			body, gerr := req.GetBody()
			if gerr != nil {
				return resp, err
			}
			req.Body = body
		}

		resp, err = t.base.RoundTrip(req)
		if attempt >= t.cfg.MaxRetries || !retryable(resp, err) {
			return resp, err
		}

		// Read Retry-After (a header, so still available) before draining the body.
		wait := backoffFor(resp, delay)
		// Drain and close the discarded response so the connection can be reused;
		// otherwise every retry against a verbose 5xx costs a fresh TCP and TLS
		// handshake, aimed at an upstream that is already failing.
		if resp != nil {
			drainClose(resp.Body)
		}

		select {
		case <-time.After(wait):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}

		if delay < t.cfg.MaxDelay {
			if delay *= 2; delay > t.cfg.MaxDelay {
				delay = t.cfg.MaxDelay
			}
		}
	}
}

// idempotentMethod reports whether method is safe to retry. An empty method
// defaults to GET in net/http, so it counts as idempotent.
func idempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, "":
		return true
	default:
		return false
	}
}

// retryable reports whether a (resp, err) outcome is a transient failure worth
// retrying. A cancelled or expired context is not retried — the caller (or the
// client's overall timeout) has already given up.
func retryable(resp *http.Response, err error) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		return true
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode >= 500 && resp.StatusCode <= 599)
}

// backoffFor returns how long to wait before the next attempt: a server-provided
// Retry-After when present and valid, otherwise delay with ±50% jitter.
func backoffFor(resp *http.Response, delay time.Duration) time.Duration {
	if resp != nil {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			return d
		}
	}
	if delay <= 0 {
		return 0
	}
	half := delay / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// parseRetryAfter parses a Retry-After header — an integer number of seconds or
// an HTTP date. It returns ok=false when the header is absent or unparseable.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true // a past date means "retry now"
	}
	return 0, false
}
