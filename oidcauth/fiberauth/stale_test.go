package fiberauth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/oidcauth"
)

// Pointing the service at another identity provider must retire the sessions
// the old one made, as it already does for API tokens.
func TestASessionFromAnotherLoginIsRefused(t *testing.T) {
	store := newMemStore()
	sess := liveSession(true)
	sess.Issuer = "https://old-id.example.com"
	store.put("tok", sess)
	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)

	resp := do(t, probeApp(g), withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "tok"))
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "anon" {
		t.Errorf("body = %q, want anon: the session was made under another login", body)
	}
}

func TestSignInRecordsTheLoginItWasMadeUnder(t *testing.T) {
	store := newMemStore()
	provider := testProvider(t, "http://app.test")
	g := newGate(t, newTestApp(t), provider, store)

	app := fiber.New()
	app.Get("/issue", func(c fiber.Ctx) error {
		return g.issueSession(c, oidcauth.Identity{Subject: "u-1"}, provider.Issuer(), false)
	})
	do(t, app, httptest.NewRequest(http.MethodGet, "/issue", nil))

	list, _ := store.ListSessions(t.Context())
	if len(list) != 1 || list[0].Issuer != provider.Issuer() {
		t.Fatalf("stored sessions = %+v, want one under %q", list, provider.Issuer())
	}
}

func tokenGate(t *testing.T, maxAge time.Duration) *Gate {
	t.Helper()
	return New(newTestApp(t), Options{
		Provider:    func() *oidcauth.Provider { return nil },
		RateLimit:   -1,
		TokenMaxAge: maxAge,
	})
}

// A token keeps the groups it was made with, so one that never expires keeps
// access for good after its maker loses it. Unset therefore means a year.
func TestATokenExpiresWithinAYearUnlessToldOtherwise(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	got := tokenGate(t, 0).tokenExpiry(now, nil)
	if got == nil || !got.Equal(now.Add(DefaultTokenMaxAge)) {
		t.Errorf("expiry with TokenMaxAge unset = %v, want %v", got, now.Add(DefaultTokenMaxAge))
	}
}

func TestANegativeTokenMaxAgeLetsATokenLiveForever(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	if got := tokenGate(t, -1).tokenExpiry(now, nil); got != nil {
		t.Errorf("expiry with a negative TokenMaxAge = %v, want none", got)
	}
}
