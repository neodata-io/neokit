package httpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoJSON_SuccessSendsAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["ping"] != "pong" {
			t.Errorf("request body = %v, want ping=pong", in)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
	}))
	defer srv.Close()

	bc := NewBaseClient(srv.URL, "test", nil)
	var out map[string]string
	if err := bc.DoJSON(context.Background(), http.MethodPost, bc.URL("/x"), map[string]string{"ping": "pong"}, &out); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if out["ok"] != "yes" {
		t.Errorf("decoded = %v, want ok=yes", out)
	}
}

func TestDoJSON_Non2xxReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	bc := NewBaseClient(srv.URL, "svc", nil)
	err := bc.DoJSON(context.Background(), http.MethodGet, bc.URL("/x"), nil, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not *APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
	if apiErr.Body != "boom" {
		t.Errorf("Body = %q, want boom", apiErr.Body)
	}
	if want := "svc API 500: boom"; apiErr.Error() != want {
		t.Errorf("Error() = %q, want %q", apiErr.Error(), want)
	}
}

func TestDoJSON_NoRetryWithoutTokenSource(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	bc := NewBaseClient(srv.URL, "test", nil)
	if err := bc.DoJSON(context.Background(), http.MethodGet, bc.URL("/x"), nil, nil); err == nil {
		t.Fatal("expected an error on 401")
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (no retry without TokenSource)", calls)
	}
}

func TestDoJSON_RefreshesAndRetriesOnce_On401(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized) // reject the first (cached) token
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-2" {
			t.Errorf("retry Authorization = %q, want Bearer token-2 (forced refresh)", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "yes"})
	}))
	defer srv.Close()

	var logins int32
	bc := NewBaseClient(srv.URL, "test", nil)
	bc.Tokens = NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		return fmt.Sprintf("token-%d", atomic.AddInt32(&logins, 1)), time.Hour, nil
	})

	var out map[string]string
	if err := bc.DoJSON(context.Background(), http.MethodGet, bc.URL("/x"), nil, &out); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (one retry)", calls)
	}
	if logins != 2 {
		t.Errorf("logins = %d, want 2 (initial + forced refresh)", logins)
	}
	if out["ok"] != "yes" {
		t.Errorf("decoded = %v, want ok=yes", out)
	}
}

func TestBearerGet_RetriesOn401(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") != "Bearer token-2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var logins int32
	ts := NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		return fmt.Sprintf("token-%d", atomic.AddInt32(&logins, 1)), time.Hour, nil
	})

	status, body, err := BearerGet(context.Background(), srv.Client(), ts, "svc", srv.URL)
	if err != nil {
		t.Fatalf("BearerGet: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 after the 401 refresh", status)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want the payload after refresh", body)
	}
	if logins != 2 {
		t.Errorf("logins = %d, want 2 (initial + forced refresh)", logins)
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (401 then 200)", calls)
	}
}

// A 204 (Spotify "nothing playing") is not a 401, so BearerGet passes the status
// straight back without a retry or a second login.
func TestBearerGet_PassesThrough204(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var logins int32
	ts := NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		return fmt.Sprintf("token-%d", atomic.AddInt32(&logins, 1)), time.Hour, nil
	})

	status, _, err := BearerGet(context.Background(), srv.Client(), ts, "svc", srv.URL)
	if err != nil {
		t.Fatalf("BearerGet: %v", err)
	}
	if status != http.StatusNoContent {
		t.Errorf("status = %d, want 204 passthrough", status)
	}
	if calls != 1 || logins != 1 {
		t.Errorf("calls=%d logins=%d, want 1/1 (no retry on a non-401)", calls, logins)
	}
}

// TestBaseClient_DebugRingOneEntryPerCall pins the shared send() core: both the
// JSON path (DoJSON) and the raw-bytes path (Bytes) must record exactly one debug
// entry per request — on success and on a non-2xx alike — so the extraction can't
// double-push or drop the outbound-request trace the admin UI relies on.
func TestBaseClient_DebugRingOneEntryPerCall(t *testing.T) {
	t.Run("DoJSON success and non-2xx", func(t *testing.T) {
		var code int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		bc := NewBaseClient(srv.URL, "svc", nil)
		bc.Debug = NewDebugRing(10)

		code = http.StatusOK
		_ = bc.DoJSON(context.Background(), http.MethodGet, bc.URL("/x"), nil, nil)
		if n := len(bc.Debug.Entries()); n != 1 {
			t.Fatalf("after one 200 DoJSON, debug entries = %d, want 1", n)
		}
		code = http.StatusInternalServerError
		_ = bc.DoJSON(context.Background(), http.MethodGet, bc.URL("/x"), nil, nil)
		if n := len(bc.Debug.Entries()); n != 2 {
			t.Fatalf("after a second (500) DoJSON, debug entries = %d, want 2", n)
		}
	})

	t.Run("Bytes success and non-2xx", func(t *testing.T) {
		var code int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write(pngBytes)
		}))
		defer srv.Close()

		bc := NewBaseClient(srv.URL, "svc", nil)
		bc.Debug = NewDebugRing(10)

		code = http.StatusOK
		_, _, _ = bc.Bytes(context.Background(), http.MethodGet, bc.URL("/art"), 0)
		if n := len(bc.Debug.Entries()); n != 1 {
			t.Fatalf("after one 200 Bytes, debug entries = %d, want 1", n)
		}
		code = http.StatusNotFound
		_, _, _ = bc.Bytes(context.Background(), http.MethodGet, bc.URL("/art"), 0)
		if n := len(bc.Debug.Entries()); n != 2 {
			t.Fatalf("after a second (404) Bytes, debug entries = %d, want 2", n)
		}
	})
}

func TestDoJSON_MarshalError(t *testing.T) {
	bc := NewBaseClient("http://example.invalid", "test", nil)
	err := bc.DoJSON(context.Background(), http.MethodPost, bc.URL("/x"), make(chan int), nil)
	if err == nil {
		t.Fatal("expected a marshal error")
	}
}

func TestDoJSON_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	bc := NewBaseClient(srv.URL, "test", nil)
	var out map[string]string
	if err := bc.DoJSON(context.Background(), http.MethodGet, bc.URL("/x"), nil, &out); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestAuthFuncs(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)

	HeaderAuth("X-Api-Key", "k")(req)
	if got := req.Header.Get("X-Api-Key"); got != "k" {
		t.Errorf("HeaderAuth set %q, want k", got)
	}
	BearerAuth("tok")(req)
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("BearerAuth set %q, want Bearer tok", got)
	}
	BasicAuth("u", "p")(req)
	if u, p, ok := req.BasicAuth(); !ok || u != "u" || p != "p" {
		t.Errorf("BasicAuth = %q/%q ok=%v, want u/p true", u, p, ok)
	}
}

func TestURL(t *testing.T) {
	bc := NewBaseClient("http://host:8080/", "test", nil) // trailing slash trimmed
	if got := bc.URL("/api/x"); got != "http://host:8080/api/x" {
		t.Errorf("URL = %q", got)
	}
	if got := bc.URL("/api/%s/%d", "u", 7); got != "http://host:8080/api/u/7" {
		t.Errorf("URL with args = %q", got)
	}
}

func TestCachingTokenSource_CachesUntilForced(t *testing.T) {
	var logins int32
	ts := NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		return fmt.Sprintf("t%d", atomic.AddInt32(&logins, 1)), time.Hour, nil
	})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if tok, _ := ts.Token(ctx, ""); tok != "t1" {
			t.Fatalf("call %d token = %q, want t1 (cached)", i, tok)
		}
	}
	if logins != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}

	// Passing the stale token (still the cached "t1") forces a refresh.
	if tok, _ := ts.Token(ctx, "t1"); tok != "t2" {
		t.Fatalf("forced token = %q, want t2", tok)
	}
	if logins != 2 {
		t.Errorf("logins after force = %d, want 2", logins)
	}
}

func TestCachingTokenSource_RefreshesAfterExpiry(t *testing.T) {
	var logins int32
	ts := NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		return fmt.Sprintf("t%d", atomic.AddInt32(&logins, 1)), 20 * time.Millisecond, nil
	})
	ctx := context.Background()

	if tok, _ := ts.Token(ctx, ""); tok != "t1" {
		t.Fatalf("first token = %q, want t1", tok)
	}
	// ttl (20ms) ≤ margin, so expiry is now+ttl; sleep well past it to avoid a
	// scheduling-jitter flake on a loaded runner.
	time.Sleep(200 * time.Millisecond)
	if tok, _ := ts.Token(ctx, ""); tok != "t2" {
		t.Fatalf("post-expiry token = %q, want t2", tok)
	}
}

func TestCachingTokenSource_UnknownTTLReusesUntilForced(t *testing.T) {
	var logins int32
	ts := NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		return fmt.Sprintf("t%d", atomic.AddInt32(&logins, 1)), 0, nil // ttl<=0 → unknown lifetime
	})
	ctx := context.Background()

	ts.Token(ctx, "")
	ts.Token(ctx, "")
	if logins != 1 {
		t.Errorf("logins = %d, want 1 (unknown ttl reused)", logins)
	}
	// Passing the stale token (still the cached "t1") forces a refresh.
	if _, _ = ts.Token(ctx, "t1"); logins != 2 {
		t.Errorf("logins after force = %d, want 2", logins)
	}
}

func TestCachingTokenSource_PropagatesLoginError(t *testing.T) {
	ts := NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		return "", 0, errors.New("login failed")
	})
	if _, err := ts.Token(context.Background(), ""); err == nil {
		t.Fatal("expected the login error to propagate")
	}
}

// TestCachingTokenSource_DedupesConcurrentForcedRefresh verifies the stampede
// guard: when several in-flight requests all get a 401 and force-refresh with the
// same stale token, only one re-login happens — the rest see the already-refreshed
// token and reuse it, rather than each hammering a rate-limited auth endpoint.
func TestCachingTokenSource_DedupesConcurrentForcedRefresh(t *testing.T) {
	var logins int32
	ts := NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		return fmt.Sprintf("t%d", atomic.AddInt32(&logins, 1)), time.Hour, nil
	})
	ctx := context.Background()

	stale, _ := ts.Token(ctx, "") // everyone observed "t1", then all get a 401
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ts.Token(ctx, stale)
		}()
	}
	wg.Wait()

	if logins != 2 {
		t.Errorf("logins = %d, want 2 (initial + exactly one deduped refresh)", logins)
	}
}
