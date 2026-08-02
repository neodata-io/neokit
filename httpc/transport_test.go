package httpc

import (
	"context"
	"errors"
	"io"
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
	return &http.Client{Timeout: 5 * time.Second, Transport: NewRetryTransport(nil, cfg)}
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
	rt := NewRetryTransport(base, fastRetry())

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

// A server can send any Retry-After it likes. Sleeping one out unbounded burns
// the caller's whole timeout and surfaces a context error instead of the 429 the
// server sent — and with Timeout: NoTimeout it parks the goroutine indefinitely.
// Past MaxDelay the retries must stop and the response come back intact.
func TestRetryTransport_DoesNotSleepOutAnOversizedRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "86400") // a day
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// No client timeout at all: nothing but the clamp can bound this.
	client := &http.Client{Transport: NewRetryTransport(nil, fastRetry())}

	done := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Errorf("Get: %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	select {
	case resp := <-done:
		if resp == nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("status = %d, want the 429 handed back", resp.StatusCode)
		}
		if got := resp.Header.Get("Retry-After"); got != "86400" {
			t.Errorf("Retry-After = %q, want it preserved for the caller", got)
		}
		if calls != 1 {
			t.Errorf("server calls = %d, want 1 — an oversized Retry-After must not be waited out", calls)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a Retry-After beyond MaxDelay was slept out; the goroutine is parked")
	}
}

// A retryable response whose request body cannot be replayed used to be drained
// by the retry path and then returned anyway, handing the caller a response with
// a closed body. Such a request must be passed straight through instead.
func TestRetryTransport_ReturnsAReadableBodyWhenTheRequestCannotBeReplayed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream detail the caller needs"))
	}))
	defer srv.Close()

	// A GET carrying a body from a reader net/http cannot rewind: GetBody is nil,
	// so no attempt after the first could reissue it.
	req, err := http.NewRequest(http.MethodGet, srv.URL, &unrewindableReader{})
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = nil

	resp, err := newRetryClient(fastRetry()).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the returned body: %v", err)
	}
	if string(body) != "upstream detail the caller needs" {
		t.Errorf("body = %q, want the upstream's — a drained body means the retry path consumed it", body)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 — an unreplayable request must not be retried", calls)
	}
}

// unrewindableReader is a request body net/http cannot produce a GetBody for.
type unrewindableReader struct{ done bool }

func (r *unrewindableReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	p[0] = 'x'
	return 1, nil
}
