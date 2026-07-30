package httpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewHTTPClient_Defaults(t *testing.T) {
	c := NewHTTPClient(HTTPOptions{})

	if c.Timeout != DefaultHTTPTimeout {
		t.Errorf("zero Timeout should mean DefaultHTTPTimeout, got %v", c.Timeout)
	}
	// The whole point of the constructor: you cannot end up without the retry
	// transport, which is also the seam that installs otel spans.
	if _, ok := c.Transport.(*RetryTransport); !ok {
		t.Fatalf("client must carry a RetryTransport, got %T", c.Transport)
	}
}

func TestNewHTTPClient_NoTimeout(t *testing.T) {
	c := NewHTTPClient(HTTPOptions{Timeout: NoTimeout})
	if c.Timeout != 0 {
		t.Errorf("NoTimeout should leave the client unbounded (ctx bounds it), got %v", c.Timeout)
	}
	if _, ok := c.Transport.(*RetryTransport); !ok {
		t.Error("a long-poll client still gets retries and tracing")
	}
}

func TestNewHTTPClient_CustomTimeoutAndTransport(t *testing.T) {
	base := &recordingTransport{}
	c := NewHTTPClient(HTTPOptions{Timeout: 3 * time.Minute, Transport: base})

	if c.Timeout != 3*time.Minute {
		t.Errorf("Timeout = %v, want 3m", c.Timeout)
	}
	// The base transport must be *wrapped*, never replaced — otherwise a custom
	// TLS config would come at the cost of retries and tracing.
	if _, ok := c.Transport.(*RetryTransport); !ok {
		t.Fatalf("custom transport must be wrapped by RetryTransport, got %T", c.Transport)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if _, err := c.Transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !base.called {
		t.Error("the supplied base transport was never reached — it was replaced, not wrapped")
	}
}

type recordingTransport struct{ called bool }

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.called = true
	return http.DefaultTransport.RoundTrip(req)
}

// A WebSocket upgrade needs the raw hijacked connection, and both the retry and
// otel wrappers wrap the response body. Installing them here breaks the dial at
// runtime — a failure no unit test on the plugin would catch, which is exactly
// why this exception is a named constructor instead of a lint suppression.
func TestNewWebSocketHTTPClient_HasNoWrappingTransportOrTimeout(t *testing.T) {
	c := NewWebSocketHTTPClient(nil)

	if _, wrapped := c.Transport.(*RetryTransport); wrapped {
		t.Error("a WS handshake client must NOT carry RetryTransport: it wraps the response body and the upgrade cannot hijack it")
	}
	if c.Timeout != 0 {
		t.Errorf("a WS client must have no timeout — it would tear down the long-lived socket; got %v", c.Timeout)
	}
}

func TestNewWebSocketHTTPClient_AppliesTLSConfigWithoutMutatingDefault(t *testing.T) {
	c := NewWebSocketHTTPClient(&tls.Config{InsecureSkipVerify: true})

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("want *http.Transport, got %T", c.Transport)
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("TLS config was not applied")
	}
	// Cloning matters: mutating http.DefaultTransport would disable certificate
	// verification for every other plugin in the process.
	if def, _ := http.DefaultTransport.(*http.Transport); def.TLSClientConfig != nil && def.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("http.DefaultTransport was mutated — InsecureSkipVerify leaked process-wide")
	}
}

func TestInsecureTLSTransport(t *testing.T) {
	tr := InsecureTLSTransport()
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureTLSTransport must disable certificate verification")
	}
	// Cloning matters: mutating http.DefaultTransport would disable verification for
	// every other plugin in the process.
	if def, _ := http.DefaultTransport.(*http.Transport); def.TLSClientConfig != nil && def.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("http.DefaultTransport was mutated — InsecureSkipVerify leaked process-wide")
	}
	// Proxy support and pooling survive the clone (a bare transport would drop them).
	if tr.Proxy == nil {
		t.Error("clone lost the default transport's proxy support")
	}
}

func TestCheckStatus(t *testing.T) {
	t.Run("2xx returns nil and leaves the body intact", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer srv.Close()

		resp, err := http.Get(srv.URL) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if err := CheckStatus("svc", resp); err != nil {
			t.Fatalf("2xx should not error: %v", err)
		}
		body, _ := ReadAllLimited(resp.Body, 0)
		if string(body) != `{"ok":true}` {
			t.Errorf("CheckStatus consumed the body on success; caller got %q", body)
		}
	})

	t.Run("non-2xx yields an APIError that errors.As can branch on", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte("already exists"))
		}))
		defer srv.Close()

		resp, err := http.Get(srv.URL) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		err = CheckStatus("svc", resp)
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("want *APIError, got %T (%v)", err, err)
		}
		if apiErr.StatusCode != http.StatusConflict || apiErr.Service != "svc" {
			t.Errorf("got %+v", apiErr)
		}
		// This is the payoff: a hand-rolled client now participates in the shared
		// conflict contract instead of stringifying its status.
		if !IsConflict(err) {
			t.Error("IsConflict should recognise a 409 from CheckStatus")
		}
	})
}

// StatusError exists for clients whose helper returns a bare (status, body) — they
// can't use CheckStatus, and hand-rolling an &APIError{} in the plugin is the drift
// the SDK is here to prevent. It has to participate in the same contract.
func TestStatusError(t *testing.T) {
	err := fmt.Errorf("myservice fetch: %w", StatusError("myservice", http.StatusConflict))

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError through the wrap, got %T", err)
	}
	if apiErr.StatusCode != http.StatusConflict || apiErr.Service != "myservice" {
		t.Errorf("got %+v", apiErr)
	}
	if !IsConflict(err) {
		t.Error("IsConflict must recognise a 409 from StatusError")
	}
	// No response body is available at these call sites, and an upstream body can
	// carry credentials — so it stays empty, and the message must not dangle.
	if got := StatusError("myservice", 403).Error(); got != "myservice API 403" {
		t.Errorf("body-less APIError should not render a trailing colon: %q", got)
	}
}

func TestRedact_StripsCredentialsFromURLErrors(t *testing.T) {
	const secret = "SECRET-API-TOKEN"

	t.Run("query string and userinfo are dropped", func(t *testing.T) {
		err := &url.Error{
			Op:  "Get",
			URL: "https://user:pw@api.example.com/prices?securityToken=" + secret + "&period=day",
			Err: errors.New("connection refused"),
		}

		got := Redact(err).Error()
		if strings.Contains(got, secret) {
			t.Errorf("token survived redaction: %s", got)
		}
		if strings.Contains(got, "pw") {
			t.Errorf("userinfo survived redaction: %s", got)
		}
		// Still useful: you must be able to see *which* upstream failed and why.
		if !strings.Contains(got, "api.example.com/prices") || !strings.Contains(got, "connection refused") {
			t.Errorf("redaction destroyed the diagnostic value: %s", got)
		}
	})

	t.Run("wrapped url.Error is still found", func(t *testing.T) {
		inner := &url.Error{Op: "Get", URL: "https://x.test/?k=" + secret, Err: errors.New("timeout")}
		if strings.Contains(Redact(inner).Error(), secret) {
			t.Error("token survived redaction")
		}
	})

	t.Run("non-url errors pass through untouched", func(t *testing.T) {
		plain := errors.New("some failure")
		if Redact(plain) != plain {
			t.Error("Redact should be a no-op for errors carrying no URL")
		}
	})

	t.Run("errors.Is still works through redaction", func(t *testing.T) {
		sentinel := errors.New("refused")
		err := Redact(&url.Error{Op: "Get", URL: "https://x.test/?k=" + secret, Err: sentinel})
		if !errors.Is(err, sentinel) {
			t.Error("redaction must preserve the error chain")
		}
	})
}
