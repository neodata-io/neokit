package httpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neodata-io/neokit/errs"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Fault
	}{
		{"nil is not a fault", nil, FaultNone},

		// The sentinels win over any status the error happened to arrive on: a plugin
		// that says "no such account" is more precise than the 404 it rode in on.
		{"wrapped ErrNotFound", fmt.Errorf("plex: user 7: %w", errs.ErrNotFound), FaultNotFound},
		{"wrapped ErrAlreadyExists", fmt.Errorf("jellyfin: %w", ErrAlreadyExists), FaultConflict},

		// This is the payoff: a bad credential is now distinguishable from an outage,
		// which is the one thing the admin actually needs to know.
		{"401 is the admin's problem", &APIError{StatusCode: http.StatusUnauthorized}, FaultAuth},
		{"403 is the admin's problem", &APIError{StatusCode: http.StatusForbidden}, FaultAuth},
		{"404", &APIError{StatusCode: http.StatusNotFound}, FaultNotFound},
		{"409", &APIError{StatusCode: http.StatusConflict}, FaultConflict},
		{"429 is not an outage", &APIError{StatusCode: http.StatusTooManyRequests}, FaultRateLimited},
		{"501 is a permanent no", &APIError{StatusCode: http.StatusNotImplemented}, FaultUnsupported},
		{"500", &APIError{StatusCode: http.StatusInternalServerError}, FaultUnavailable},
		{"502", &APIError{StatusCode: http.StatusBadGateway}, FaultUnavailable},

		// An APIError reached through a wrap still classifies — plugins wrap on the
		// way out ("qbittorrent GET /x: %w"), so this is the common case, not an edge.
		{"wrapped APIError", fmt.Errorf("adguard status: %w", &APIError{StatusCode: 401}), FaultAuth},

		// Nothing reached the service.
		{"dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, FaultUnavailable},
		{"deadline", context.DeadlineExceeded, FaultUnavailable},

		// A cancelled context is us shutting down, not the service failing.
		{"cancelled", context.Canceled, FaultUnavailable},

		// Deliberately NOT guessed as transient: an error nobody has classified is
		// more likely a bug than a blip, and retrying it forever would hide it.
		{"an error we can't place", errors.New("something odd"), FaultUnknown},
		{"a 400 we can't act on", &APIError{StatusCode: http.StatusBadRequest}, FaultUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// A scheduler asks exactly one question of a Fault: try again, or stop? Getting
// this wrong is expensive in both directions — retrying a wrong password hammers a
// rate limit, and giving up on a brief blip leaves a tile dark until a restart.
func TestFault_Retryable(t *testing.T) {
	retryable := map[Fault]bool{
		FaultRateLimited: true,
		FaultUnavailable: true,

		FaultAuth:        false, // the credential is wrong; trying again cannot fix it
		FaultConflict:    false, // needs a human decision
		FaultNotFound:    false,
		FaultUnsupported: false, // a permanent no
		FaultUnknown:     false, // conservative: don't retry what we don't understand
		FaultNone:        false,
	}

	for f, want := range retryable {
		if got := f.Retryable(); got != want {
			t.Errorf("%q.Retryable() = %v, want %v", f, got, want)
		}
	}
}

// The badge the admin sees. "Check your API key" is worth surfacing; "the NAS is
// briefly down" is not — the household cannot do anything about it, and a monitor
// that cries wolf is one nobody reads.
func TestFault_AdminActionable(t *testing.T) {
	if !FaultAuth.AdminActionable() {
		t.Error("FaultAuth must be admin-actionable: a wrong credential is exactly what an operator can fix")
	}
	if FaultUnavailable.AdminActionable() {
		t.Error("FaultUnavailable must not be admin-actionable: nobody can fix an upstream being down")
	}
	if FaultUnknown.AdminActionable() {
		t.Error("FaultUnknown must not be admin-actionable: we cannot tell the admin what to do about it")
	}
}

// Every failure of http.Client.Do is a *url.Error, and *url.Error is a
// net.Error — so "is a net.Error" called all of these transient, and a
// scheduler retried a wrong certificate forever at Debug level.
func TestClassify_APermanentTransportFailureIsNotUnavailable(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer untrusted.Close()

	once := &RetryConfig{}
	cases := []struct {
		name   string
		client *http.Client
		url    string
	}{
		{"guard refusal", NewHTTPClient(HTTPOptions{Transport: GuardedTransport(nil), Retry: once}), plain.URL},
		{"certificate failure", NewHTTPClient(HTTPOptions{Transport: &http.Transport{}, Retry: once}), untrusted.URL},
		{"unsupported scheme", NewHTTPClient(HTTPOptions{Retry: once}), "gopher://example.com/x"},
	}
	for _, c := range cases {
		resp, err := c.client.Get(c.url)
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("%s: want an error", c.name)
			continue
		}
		if got := Classify(err); got != FaultUnknown {
			t.Errorf("%s: Classify(%v) = %q, want %q", c.name, err, got, FaultUnknown)
		}
	}
}

func TestClassify_ARefusedConnectionIsStillUnavailable(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	_, err := NewHTTPClient(HTTPOptions{Retry: &RetryConfig{}}).Get(url) //nolint:bodyclose // err path returns no body
	if got := Classify(err); got != FaultUnavailable {
		t.Errorf("Classify(%v) = %q, want %q", err, got, FaultUnavailable)
	}
}
