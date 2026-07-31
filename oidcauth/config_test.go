package oidcauth

import (
	"reflect"
	"testing"
	"time"
)

func TestNormalizeCanonicalisesURLsAndFillsDefaults(t *testing.T) {
	c := Config{
		Issuer:       "  https://id.example.com/ ",
		ClientID:     " app ",
		ClientSecret: " s3cret ",
		BaseURL:      "https://app.example.com/",
		OwnerGroup:   " admins ",
	}
	c.Normalize()

	// A pasted trailing slash must not produce a different provider or a
	// different redirect URI than the same value without one.
	if c.Issuer != "https://id.example.com" {
		t.Errorf("Issuer = %q", c.Issuer)
	}
	if c.BaseURL != "https://app.example.com" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.ClientID != "app" || c.ClientSecret != "s3cret" || c.OwnerGroup != "admins" {
		t.Errorf("values not trimmed: %+v", c)
	}
	if !reflect.DeepEqual(c.Scopes, DefaultScopes) {
		t.Errorf("Scopes = %v, want the defaults", c.Scopes)
	}
	if c.GroupsClaim != DefaultGroupsClaim || c.CallbackPath != DefaultCallbackPath || c.PostLogoutPath != DefaultPostLogoutPath {
		t.Errorf("defaults not filled: %+v", c)
	}
}

// A path written without its leading slash must still produce a well-formed URL
// when appended to BaseURL, rather than "https://app.examplecallback".
func TestNormalizeAddsALeadingSlashToPaths(t *testing.T) {
	c := Config{Issuer: "i", ClientID: "c", ClientSecret: "s", BaseURL: "https://a.test", CallbackPath: "cb"}
	c.Normalize()
	if c.CallbackPath != "/cb" {
		t.Errorf("CallbackPath = %q, want /cb", c.CallbackPath)
	}
}

// New declining on an incomplete config is how an application makes its login
// gate optional without a second "enabled" flag that can disagree.
func TestNewDeclinesUnlessFullyConfigured(t *testing.T) {
	full := Config{Issuer: "https://id.test", ClientID: "c", ClientSecret: "s", BaseURL: "https://a.test"}
	if _, ok := New(full); !ok {
		t.Fatal("New declined a complete config")
	}
	for _, blank := range []string{"Issuer", "ClientID", "ClientSecret", "BaseURL"} {
		t.Run("missing "+blank, func(t *testing.T) {
			c := full
			reflect.ValueOf(&c).Elem().FieldByName(blank).SetString("   ")
			if c.Configured() {
				t.Error("Configured must be false with a blank required field")
			}
			if _, ok := New(c); ok {
				t.Error("New must decline an incomplete config")
			}
		})
	}
}

// The Secure flag must come from the configured origin, not from the request:
// behind a TLS-terminating proxy the request arrives over plain HTTP, and every
// scheme-sniffing heuristic drops Secure on exactly the deployments that need it.
func TestCookieSecureFollowsTheConfiguredBaseURL(t *testing.T) {
	for base, want := range map[string]bool{
		"https://app.example.com":  true,
		"http://192.168.1.10:8080": false,
	} {
		p, ok := New(Config{Issuer: "https://id.test", ClientID: "c", ClientSecret: "s", BaseURL: base})
		if !ok {
			t.Fatalf("New declined %q", base)
		}
		if got := p.CookieSecure(); got != want {
			t.Errorf("CookieSecure() for %q = %v, want %v", base, got, want)
		}
	}
}

func TestRedirectURIComposesBaseAndCallback(t *testing.T) {
	p, _ := New(Config{
		Issuer: "https://id.test", ClientID: "c", ClientSecret: "s",
		BaseURL: "https://app.example.com/", CallbackPath: "/oauth/cb",
	})
	if got := p.RedirectURI(); got != "https://app.example.com/oauth/cb" {
		t.Errorf("RedirectURI = %q", got)
	}
	if got := p.PostLogoutURI(); got != "https://app.example.com/" {
		t.Errorf("PostLogoutURI = %q", got)
	}
}

// ── Policy ──────────────────────────────────────────────────────────────────

func TestPolicyLiveRejectsExpiredAndOverAgedSessions(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	p := Policy{TTL: time.Hour, MaxLifetime: 24 * time.Hour, TouchInterval: time.Minute}

	cases := []struct {
		name string
		sess Session
		want bool
	}{
		{"fresh", Session{CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}, true},
		{"idle-expired", Session{CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(-time.Second)}, false},
		{"expiring exactly now", Session{CreatedAt: now.Add(-time.Minute), ExpiresAt: now}, false},
		// The cap is the real bound on how long a revoked identity keeps access,
		// because Owner and Groups are frozen at login and never re-derived.
		{"over the absolute cap", Session{CreatedAt: now.Add(-25 * time.Hour), ExpiresAt: now.Add(time.Hour)}, false},
		// A row written before the cap existed has no CreatedAt; it must be retired
		// by the TTL rather than rejected outright.
		{"zero CreatedAt falls back to the TTL", Session{ExpiresAt: now.Add(time.Hour)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Live(tc.sess, now); got != tc.want {
				t.Errorf("Live = %v, want %v", got, tc.want)
			}
		})
	}
}

// Clamping the renewal to the cap is what lets a sweeper collect the row on
// schedule. Without it a renewal pushes expires_at past the cap, leaving the row
// rejected at read time but never *expired* — unreachable and uncollectable.
func TestExpiryForClampsToTheAbsoluteCap(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	p := Policy{TTL: 30 * 24 * time.Hour, MaxLifetime: 24 * time.Hour, TouchInterval: time.Hour}

	created := now.Add(-20 * time.Hour)
	got := p.ExpiryFor(created, now)
	want := created.Add(24 * time.Hour) // the cap, not now+TTL
	if !got.Equal(want) {
		t.Errorf("ExpiryFor = %v, want the hard cap %v", got, want)
	}
	if got.After(created.Add(p.MaxLifetime)) {
		t.Error("a renewal must never push expiry past the absolute cap")
	}
}

// A caller overriding one field must not lose the other two.
func TestPolicyZeroFieldsFallBackToDefaults(t *testing.T) {
	p := Policy{TTL: time.Hour} // MaxLifetime and TouchInterval left zero
	d := DefaultPolicy()
	now := time.Now()

	// TouchInterval defaulted: a session seen just now is not due a write.
	if p.NeedsTouch(Session{LastSeenAt: now}, now) {
		t.Error("a just-seen session must not need a touch")
	}
	if !p.NeedsTouch(Session{LastSeenAt: now.Add(-2 * d.TouchInterval)}, now) {
		t.Error("a long-idle session must need a touch")
	}
	// MaxLifetime defaulted rather than treated as zero, which would expire
	// every session instantly.
	if !p.Live(Session{CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}, now) {
		t.Error("a zero MaxLifetime must fall back to the default, not expire everything")
	}
}

// ── Handshake ───────────────────────────────────────────────────────────────

func TestNewHandshakeMintsDistinctSecrets(t *testing.T) {
	h, err := NewHandshake()
	if err != nil {
		t.Fatalf("NewHandshake: %v", err)
	}
	if !h.Valid() {
		t.Fatal("a fresh handshake must be valid")
	}
	if h.State == h.Nonce || h.Nonce == h.Verifier || h.State == h.Verifier {
		t.Error("the three secrets must be independent")
	}
	// RFC 7636 §4.1 requires a 43–128 character verifier.
	if len(h.Verifier) < 43 || len(h.Verifier) > 128 {
		t.Errorf("PKCE verifier length %d is outside RFC 7636's 43–128", len(h.Verifier))
	}
	other, _ := NewHandshake()
	if other.State == h.State {
		t.Error("two handshakes must not share a state")
	}
}

// A blank nonce silently disables the replay check, so Valid must refuse one.
func TestHandshakeValidRequiresAllThree(t *testing.T) {
	for name, h := range map[string]Handshake{
		"no state":    {Nonce: "n", Verifier: "v"},
		"no nonce":    {State: "s", Verifier: "v"},
		"no verifier": {State: "s", Nonce: "n"},
	} {
		if h.Valid() {
			t.Errorf("%s: Valid must be false", name)
		}
	}
}

func TestHashTokenIsStableAndNotTheToken(t *testing.T) {
	const token = "a-session-token"
	h := HashToken(token)
	if h == token {
		t.Fatal("the stored hash must not be the token")
	}
	if h != HashToken(token) {
		t.Fatal("HashToken must be deterministic")
	}
	if len(h) != 64 {
		t.Errorf("want a hex sha256 (64 chars), got %d", len(h))
	}
}
