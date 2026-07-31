package httpc

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// artClient is the documented shape for a caller that holds only a bare
// *http.Client: a zero BaseClient, no BaseURL. These tests exercise it directly
// so the migration path in [BaseClient.Image]'s doc stays honest.
func artClient(hc *http.Client, auth AuthFunc) *BaseClient {
	return &BaseClient{HTTPClient: hc, Service: "art", Auth: auth}
}

func TestImage_SuccessAndContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	blob, err := artClient(srv.Client(), nil).Image(context.Background(), srv.URL, 0)
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	if !bytes.Equal(blob.Data, pngBytes) {
		t.Errorf("data = %v, want %v", blob.Data, pngBytes)
	}
	if blob.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", blob.ContentType)
	}
}

func TestImage_DefaultsContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "") // force empty
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	blob, err := artClient(srv.Client(), nil).Image(context.Background(), srv.URL, 0)
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	if blob.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg default", blob.ContentType)
	}
}

func TestImage_AppliesAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != "sekret" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	// Without auth → 403 → error.
	if _, err := artClient(srv.Client(), nil).Image(context.Background(), srv.URL, 0); err == nil {
		t.Fatal("expected error without auth")
	}
	// With the auth hook → 200.
	blob, err := artClient(srv.Client(), HeaderAuth("X-Token", "sekret")).
		Image(context.Background(), srv.URL, 0)
	if err != nil {
		t.Fatalf("Image with auth: %v", err)
	}
	if !bytes.Equal(blob.Data, pngBytes) {
		t.Error("auth request did not return the image")
	}
}

func TestImage_EnforcesSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 100))
	}))
	defer srv.Close()

	if _, err := artClient(srv.Client(), nil).Image(context.Background(), srv.URL, 10); err == nil {
		t.Fatal("expected an error when the body exceeds maxBytes")
	}
}

func TestImage_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := artClient(srv.Client(), nil).Image(context.Background(), srv.URL, 0); err == nil {
		t.Fatal("expected an error for a 404")
	}
}

// An art fetch must fail with a *APIError rather than a stringified "HTTP %d":
// only then does Classify walk the chain and tell the caller whose problem it is.
func TestImage_ClassifiableError(t *testing.T) {
	cases := []struct {
		status int
		want   Fault
	}{
		{http.StatusNotFound, FaultNotFound},
		{http.StatusTooManyRequests, FaultRateLimited},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
		}))
		_, err := artClient(srv.Client(), nil).Image(context.Background(), srv.URL, 0)
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected an error", c.status)
		}
		if got := Classify(err); got != c.want {
			t.Errorf("status %d: Classify = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestBaseClientImage_AppliesAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	bc := NewBaseClient(srv.URL, "svc", HeaderAuth("X-Api-Key", "k"))
	blob, err := bc.Image(context.Background(), bc.URL("/art"), 0)
	if err != nil {
		t.Fatalf("Image: %v", err)
	}
	if blob.ContentType != "image/webp" || !bytes.Equal(blob.Data, pngBytes) {
		t.Errorf("unexpected blob: ct=%q len=%d", blob.ContentType, len(blob.Data))
	}
}

func TestBaseClientBytes_RetriesOn401(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer token-2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	logins := 0
	bc := NewBaseClient(srv.URL, "svc", nil)
	bc.Tokens = NewCachingTokenSource(func(context.Context) (string, time.Duration, error) {
		logins++
		return fmt.Sprintf("token-%d", logins), time.Hour, nil
	})

	data, _, err := bc.Bytes(context.Background(), http.MethodGet, bc.URL("/art"), 0)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !bytes.Equal(data, pngBytes) {
		t.Error("did not return the image after 401 refresh")
	}
	if logins != 2 {
		t.Errorf("logins = %d, want 2 (initial + one forced refresh)", logins)
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (401 then 200)", calls)
	}
}
