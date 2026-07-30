package httpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry is the default policy with negligible backoff so tests don't sleep.
func fastRetry() RetryConfig {
	return RetryConfig{MaxRetries: 2, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}
}

func newRetryClient(cfg RetryConfig) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: NewRetryTransportConfig(nil, cfg)}
}

func TestRetryTransport_RetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // fail twice
			return
		}
		w.WriteHeader(http.StatusOK) // succeed on the third
	}))
	defer srv.Close()

	resp, err := newRetryClient(fastRetry()).Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("server calls = %d, want 3 (initial + 2 retries)", calls)
	}
}

func TestRetryTransport_ExhaustsAndReturnsLast5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	resp, err := newRetryClient(fastRetry()).Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (last response surfaced)", resp.StatusCode)
	}
	if calls != 3 {
		t.Errorf("server calls = %d, want 3 (initial + 2 retries)", calls)
	}
}

func TestRetryTransport_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	resp, err := newRetryClient(fastRetry()).Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (4xx is not retried)", calls)
	}
}

func TestRetryTransport_NoRetryOnPost(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	resp, err := newRetryClient(fastRetry()).Post(srv.URL, "text/plain", http.NoBody)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (POST is non-idempotent, never retried)", calls)
	}
}

func TestRetryTransport_HonorsRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0") // retry immediately
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resp, err := newRetryClient(fastRetry()).Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (429 retried after Retry-After)", calls)
	}
}

func TestRetryTransport_BackoffRespectsContextCancel(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable) // always fail → would keep retrying
	}))
	defer srv.Close()

	// A long backoff so the cancel lands during the wait, not between requests.
	cfg := RetryConfig{MaxRetries: 5, BaseDelay: time.Second, MaxDelay: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	_, err := newRetryClient(cfg).Do(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (cancelled during first backoff)", calls)
	}
}

// stubRT is a base RoundTripper that fails with a transport error a fixed number
// of times before succeeding — used to exercise the network-error retry path
// without a flaky real connection.
type stubRT struct {
	failuresLeft int32
	hits         int32
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&s.hits, 1)
	if atomic.AddInt32(&s.failuresLeft, -1) >= 0 {
		return nil, errors.New("dial tcp: connection refused")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

func TestRetryTransport_RetriesTransportError(t *testing.T) {
	base := &stubRT{failuresLeft: 2}
	rt := NewRetryTransportConfig(base, fastRetry())

	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if base.hits != 3 {
		t.Errorf("base hits = %d, want 3 (2 transport errors + success)", base.hits)
	}
}

func TestRetryTransport_DisabledWhenMaxRetriesZero(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := RetryConfig{MaxRetries: 0, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	resp, err := newRetryClient(cfg).Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	resp.Body.Close()
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (retries disabled)", calls)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("5"); !ok || d != 5*time.Second {
		t.Errorf("parseRetryAfter(5) = %v, %v; want 5s, true", d, ok)
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Errorf("parseRetryAfter(empty) ok = true, want false")
	}
	if _, ok := parseRetryAfter("-3"); ok {
		t.Errorf("parseRetryAfter(-3) ok = true, want false")
	}
	if _, ok := parseRetryAfter("garbage"); ok {
		t.Errorf("parseRetryAfter(garbage) ok = true, want false")
	}
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(future); !ok || d <= 0 {
		t.Errorf("parseRetryAfter(future date) = %v, %v; want >0, true", d, ok)
	}
}

func TestIdempotentMethod(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, ""} {
		if !idempotentMethod(m) {
			t.Errorf("idempotentMethod(%q) = false, want true", m)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if idempotentMethod(m) {
			t.Errorf("idempotentMethod(%q) = true, want false", m)
		}
	}
}

func TestRetryable(t *testing.T) {
	if retryable(nil, context.Canceled) {
		t.Error("context.Canceled should not be retryable")
	}
	if retryable(nil, context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded should not be retryable")
	}
	if !retryable(nil, errors.New("connection reset")) {
		t.Error("a transport error should be retryable")
	}
	if !retryable(&http.Response{StatusCode: 503}, nil) {
		t.Error("503 should be retryable")
	}
	if !retryable(&http.Response{StatusCode: 429}, nil) {
		t.Error("429 should be retryable")
	}
	if retryable(&http.Response{StatusCode: 404}, nil) {
		t.Error("404 should not be retryable")
	}
	if retryable(&http.Response{StatusCode: 200}, nil) {
		t.Error("200 should not be retryable")
	}
}
