package fiberauth

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/oidcauth"
)

// A login with no owner group grants nobody admin rights, which an operator
// should hear about at boot rather than discover at the first 403.
func TestALoginWithoutAnOwnerGroupIsReportedAtBoot(t *testing.T) {
	p, ok := oidcauth.New(oidcauth.Config{
		Issuer: "https://id.example.com", ClientID: "app", ClientSecret: "s", BaseURL: "http://app.test",
	})
	if !ok {
		t.Fatal("New declined a complete config")
	}
	var buf bytes.Buffer
	New(newTestApp(t), Options{
		Provider: func() *oidcauth.Provider { return p },
		Sessions: newMemStore(),
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if !strings.Contains(buf.String(), "no one is an owner") {
		t.Errorf("log = %q, want a warning that no one is an owner", buf.String())
	}

	buf.Reset()
	withGroup := testProvider(t, "http://app.test")
	New(newTestApp(t), Options{
		Provider: func() *oidcauth.Provider { return withGroup },
		Sessions: newMemStore(),
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if strings.Contains(buf.String(), "owner") {
		t.Errorf("log = %q, want no owner warning when an owner group is set", buf.String())
	}
}

// The User-Agent comes from Fiber's pooled request buffer: a store that keeps
// the session struct must not see it change when a later request reuses it.
func TestTheStoredUserAgentDoesNotChangeWithALaterRequest(t *testing.T) {
	store := newMemStore()
	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)
	app := fiber.New()
	app.Get("/issue", func(c fiber.Ctx) error {
		return g.issueSession(c, oidcauth.Identity{Subject: "u-1"}, "https://id.example.com", false)
	})
	app.Get("/other", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	first := httptest.NewRequest(http.MethodGet, "/issue", nil)
	first.Header.Set("User-Agent", strings.Repeat("A", 64))
	do(t, app, first)
	for range 20 {
		later := httptest.NewRequest(http.MethodGet, "/other", nil)
		later.Header.Set("User-Agent", strings.Repeat("B", 64))
		do(t, app, later)
	}

	list, _ := store.ListSessions(t.Context())
	if len(list) != 1 || list[0].UserAgent != strings.Repeat("A", 64) {
		t.Errorf("stored sessions = %+v, want the signing-in request's user agent", list)
	}
}

// Renewal moves the expiry in the store; unless the cookie moves with it, the
// browser drops the session at the original expiry however active it is.
func TestARenewedSessionSendsTheCookieWithTheNewExpiry(t *testing.T) {
	now := time.Now().UTC()
	store := newMemStore()
	s := liveSession(true)
	s.CreatedAt = now.Add(-24 * time.Hour)
	s.LastSeenAt = now.Add(-2 * time.Hour) // stale enough to be touched
	s.ExpiresAt = now.Add(time.Hour)
	store.put("tok", s)
	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)

	resp := do(t, probeApp(g), withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "tok"))
	var renewed *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "myapp_session" {
			renewed = c
		}
	}
	if renewed == nil {
		t.Fatal("a renewed session must re-send its cookie")
	}
	if renewed.Value != "tok" || !renewed.HttpOnly || renewed.SameSite != http.SameSiteLaxMode {
		t.Errorf("renewed cookie = %+v, want the same token with the same attributes", renewed)
	}
	if !renewed.Expires.After(now.Add(24 * time.Hour)) {
		t.Errorf("cookie expires %v, want the renewed expiry", renewed.Expires)
	}

	// A session that was not renewed leaves the cookie alone.
	store.put("fresh", liveSession(true))
	resp = do(t, probeApp(g), withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "fresh"))
	if len(resp.Cookies()) != 0 {
		t.Errorf("cookies = %v, want none for a session that was not renewed", resp.Cookies())
	}
}
