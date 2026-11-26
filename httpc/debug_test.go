package httpc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The debug ring is surfaced through an admin UI, so it must not carry the
// credential that Redact exists to keep out of logs. A client whose auth rides
// in an api_key query parameter would otherwise publish its token there.
func TestDebugRingDoesNotCaptureCredentialsFromTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ring := NewDebugRing(4)
	c := BaseClient{HTTPClient: srv.Client(), Service: "vendor", Debug: ring}

	target := srv.URL + "/things?api_key=SUPERSECRET&x=1"
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.send(req, http.MethodGet, target)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	drainClose(resp.Body)

	entries := ring.Entries()
	if len(entries) != 1 {
		t.Fatalf("ring holds %d entries, want 1", len(entries))
	}
	if strings.Contains(entries[0].URL, "SUPERSECRET") {
		t.Errorf("debug ring captured the credential: %q", entries[0].URL)
	}
	if entries[0].URL != srv.URL {
		t.Errorf("ring URL = %q, want only scheme://host %q", entries[0].URL, srv.URL)
	}
}

// Telegram bot tokens and webhook secrets ride in the path, so a path is as
// secret as a query string and the ring names only the host.
func TestDebugRingDoesNotCaptureASecretInThePath(t *testing.T) {
	const secret = "bot123456:SECRETTOKEN"
	ring := NewDebugRing(4)
	c := BaseClient{HTTPClient: http.DefaultClient, Service: "tg", Debug: ring}

	target := "http://neokit-nonexistent.invalid/" + secret + "/sendMessage"
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.send(req, http.MethodPost, target)
	if err == nil {
		t.Fatal("send must fail against an unresolvable host")
	}
	if strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("returned error carries the path secret: %v", err)
	}
	e := ring.Entries()[0]
	if strings.Contains(e.URL, "SECRETTOKEN") || strings.Contains(e.Error, "SECRETTOKEN") {
		t.Errorf("debug ring carries the path secret: %+v", e)
	}
}

// The transport-error path records err.Error(), which for a *url.Error carries
// the full URL — query string included.
func TestDebugRingDoesNotCaptureCredentialsFromATransportError(t *testing.T) {
	ring := NewDebugRing(4)
	c := BaseClient{HTTPClient: http.DefaultClient, Service: "vendor", Debug: ring}

	// A host that cannot resolve, so Do fails and the *url.Error carries the URL.
	target := "http://neokit-nonexistent.invalid/things?api_key=SUPERSECRET"
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.send(req, http.MethodGet, target); err == nil {
		t.Fatal("send must fail against an unresolvable host")
	}

	entries := ring.Entries()
	if len(entries) != 1 {
		t.Fatalf("ring holds %d entries, want 1", len(entries))
	}
	if strings.Contains(entries[0].URL, "SUPERSECRET") {
		t.Errorf("debug ring URL captured the credential: %q", entries[0].URL)
	}
	if strings.Contains(entries[0].Error, "SUPERSECRET") {
		t.Errorf("debug ring Error captured the credential: %q", entries[0].Error)
	}
}
