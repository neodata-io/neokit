package fiberauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	neoapp "github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/oidcauth"
)

// newTestApp builds the app a gate mounts itself on. Aliased to neoapp because
// these tests use `app` as a local name for the Fiber router underneath.
func newTestApp(t *testing.T) *neoapp.App {
	t.Helper()
	a, err := neoapp.New(neoapp.Options{
		Name: "testapp",
		Base: config.Base{Port: 0, LogLevel: "error", LogFormat: "json"},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func componentNamed(a *neoapp.App, name string) (neoapp.Component, bool) {
	for _, c := range a.Components() {
		if c.Name == name {
			return c, true
		}
	}
	return neoapp.Component{}, false
}

// ── Test doubles ────────────────────────────────────────────────────────────

// memStore is a deliberately naive SessionStore: it returns rows verbatim, with
// no expiry filtering of its own.
//
// That naivety is the point. SessionStore is an interface, so the expiry rule
// must live in the middleware — a store that enforces it would hide a middleware
// that does not, and the security property would silently depend on which
// implementation happened to be wired in.
type memStore struct {
	mu       sync.Mutex
	byHash   map[string]oidcauth.Session
	touched  map[string]time.Time
	failNext error
}

func newMemStore() *memStore {
	return &memStore{byHash: map[string]oidcauth.Session{}, touched: map[string]time.Time{}}
}

func (m *memStore) CreateSession(_ context.Context, s oidcauth.Session, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		return err
	}
	m.byHash[hash] = s
	return nil
}

func (m *memStore) SessionByToken(_ context.Context, hash string) (oidcauth.Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byHash[hash]
	return s, ok, nil
}

func (m *memStore) TouchSession(_ context.Context, id string, lastSeen, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touched[id] = expires
	for h, s := range m.byHash {
		if s.ID == id {
			s.LastSeenAt, s.ExpiresAt = lastSeen, expires
			m.byHash[h] = s
		}
	}
	return nil
}

func (m *memStore) DeleteSession(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, s := range m.byHash {
		if s.ID == id {
			delete(m.byHash, h)
		}
	}
	return nil
}

func (m *memStore) DeleteSessionByToken(_ context.Context, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byHash, hash)
	return nil
}

func (m *memStore) ListSessions(context.Context) ([]oidcauth.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]oidcauth.Session, 0, len(m.byHash))
	for _, s := range m.byHash {
		out = append(out, s)
	}
	return out, nil
}

// put stores a session under a token and returns the raw token.
func (m *memStore) put(token string, s oidcauth.Session) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byHash[oidcauth.HashToken(token)] = s
	return token
}

// testProvider builds a provider whose only job is to answer CookieSecure and
// RedirectURI — no network is involved.
func testProvider(t *testing.T, baseURL string) *oidcauth.Provider {
	t.Helper()
	p, ok := oidcauth.New(oidcauth.Config{
		Issuer: "https://id.example.com", ClientID: "app", ClientSecret: "s",
		BaseURL: baseURL, OwnerGroup: "admins",
	})
	if !ok {
		t.Fatal("New declined a complete config")
	}
	return p
}

// newGate wires a gate onto a. A nil provider means "login not configured".
func newGate(t *testing.T, a *neoapp.App, p *oidcauth.Provider, store oidcauth.SessionStore) *Gate {
	t.Helper()
	return New(a, Options{
		Provider:     func() *oidcauth.Provider { return p },
		Sessions:     store,
		CookiePrefix: "myapp",
		RateLimit:    -1, // off: these tests fire many requests from one peer
	})
}

// liveSession is a session that has not expired and is well inside the cap.
func liveSession(owner bool) oidcauth.Session {
	now := time.Now().UTC()
	return oidcauth.Session{
		ID: "sess-1", Subject: "u-1", Name: "Alex", Email: "alex@example.com",
		Groups: []string{"admins"}, Owner: owner,
		CreatedAt: now.Add(-time.Minute), LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}
}

// do runs one request through an app.
func do(t *testing.T, app *fiber.App, req *http.Request) *http.Response {
	t.Helper()
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

// probeApp mounts ResolveIdentity plus a guarded route that reports who got in.
func probeApp(g *Gate) *fiber.App {
	app := fiber.New()
	app.Use(g.ResolveIdentity())
	app.Get("/open", func(c fiber.Ctx) error {
		if id, ok := IdentityFrom(c); ok {
			return c.SendString("id:" + id.Subject)
		}
		return c.SendString("anon")
	})
	app.Get("/admin", g.RequireOwner(), func(c fiber.Ctx) error { return c.SendString("admin") })
	app.Get("/member", g.RequireAuth(), func(c fiber.Ctx) error { return c.SendString("member") })
	return app
}

func withCookie(req *http.Request, name, value string) *http.Request {
	req.AddCookie(&http.Cookie{Name: name, Value: value})
	return req
}

// ── The open model: no login configured ─────────────────────────────────────

// An application that ships open must stay open until credentials are set, and
// switching them off must restore that exactly. This is what makes the gate
// additive and reversible.
func TestDisabledGateLeavesEveryRouteOpen(t *testing.T) {
	g := newGate(t, newTestApp(t), nil, newMemStore())
	app := probeApp(g)

	for _, path := range []string{"/open", "/admin", "/member"} {
		resp := do(t, app, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d with no login configured, want 200 (the open model)", path, resp.StatusCode)
		}
	}
}

// With no login configured the resolver must not read a cookie or touch the
// store at all — that is the "no cost when unused" property.
func TestDisabledGateNeverTouchesTheStore(t *testing.T) {
	store := newMemStore()
	store.put("tok", liveSession(true))
	g := newGate(t, newTestApp(t), nil, store)
	app := probeApp(g)

	resp := do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "tok"))
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "anon" {
		t.Errorf("body = %q, want anon — a disabled gate must resolve nobody", body)
	}
}

// Session rows outlive the gate being switched off. Without a 404 the session
// list would become unauthenticated, readable PII the moment OIDC was disabled,
// and anyone could revoke a login — RequireOwner alone passes through here.
func TestSessionRoutes404WhenLoginIsNotConfigured(t *testing.T) {
	store := newMemStore()
	store.put("tok", liveSession(true))
	a := newTestApp(t)
	g := newGate(t, a, nil, store)
	app := a.HTTP

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, g.SessionsPath()},
		{http.MethodDelete, g.SessionsPath() + "/sess-1"},
		{http.MethodGet, g.LoginPath()},
	} {
		resp := do(t, app, httptest.NewRequest(tc.method, tc.path, nil))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 — an unconfigured deployment exposes no auth surface", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// Logout is the deliberate exception: a browser holding a cookie from before the
// login was switched off must still be able to clear it.
func TestLogoutKeepsWorkingAfterLoginIsDisabled(t *testing.T) {
	store := newMemStore()
	store.put("tok", liveSession(true))
	a := newTestApp(t)
	g := newGate(t, a, nil, store)
	app := a.HTTP

	resp := do(t, app, withCookie(httptest.NewRequest(http.MethodPost, g.LogoutPath(), nil), "myapp_session", "tok"))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", resp.StatusCode)
	}
	if _, ok, _ := store.SessionByToken(context.Background(), oidcauth.HashToken("tok")); ok {
		t.Error("logout must delete the session even with no provider configured")
	}
	if !clearsCookie(resp, "myapp_session") {
		t.Error("logout must clear the session cookie")
	}
}

// The scheme, and therefore the cookie name, can change under a live browser:
// switching the login off flips secure to false while a __Host- cookie is still
// on the client. A logout that could not clear it would be a lie.
func TestLogoutClearsEveryCookieNameItCouldHaveUsed(t *testing.T) {
	a := newTestApp(t)
	g := newGate(t, a, nil, newMemStore())
	app := a.HTTP

	resp := do(t, app, httptest.NewRequest(http.MethodPost, g.LogoutPath(), nil))
	for _, name := range []string{"myapp_session", "__Host-myapp_session"} {
		if !clearsCookie(resp, name) {
			t.Errorf("logout did not clear %q", name)
		}
	}
}

// clearsCookie reports whether the response expires the named cookie.
func clearsCookie(resp *http.Response, name string) bool {
	for _, c := range resp.Cookies() {
		if c.Name == name && (c.MaxAge < 0 || c.Value == "" && c.Expires.Before(time.Now())) {
			return true
		}
	}
	return false
}

// ── Identity resolution and the guards ──────────────────────────────────────

func TestResolveIdentityRecognisesALiveSession(t *testing.T) {
	store := newMemStore()
	store.put("tok", liveSession(true))
	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)
	app := probeApp(g)

	resp := do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "tok"))
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "id:u-1" {
		t.Errorf("body = %q, want id:u-1", body)
	}
}

func TestGuardsDistinguishAnonymousFromNonOwner(t *testing.T) {
	store := newMemStore()
	store.put("owner", liveSession(true))
	store.put("member", liveSession(false))
	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)
	app := probeApp(g)

	cases := []struct {
		name, path, token string
		want              int
	}{
		{"anonymous on admin", "/admin", "", http.StatusUnauthorized},
		{"member on admin", "/admin", "member", http.StatusForbidden},
		{"owner on admin", "/admin", "owner", http.StatusOK},
		{"anonymous on member", "/member", "", http.StatusUnauthorized},
		{"member on member", "/member", "member", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.token != "" {
				withCookie(req, "myapp_session", tc.token)
			}
			if resp := do(t, app, req); resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// The security property must not depend on which store is wired in: memStore
// returns expired rows verbatim, so only the middleware's own check can reject
// them.
func TestExpiredSessionIsRejectedEvenByANaiveStore(t *testing.T) {
	now := time.Now().UTC()
	store := newMemStore()
	s := liveSession(true)
	s.ExpiresAt = now.Add(-time.Second) // dead, but the store will still hand it over
	store.put("tok", s)

	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)
	app := probeApp(g)

	resp := do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/admin", nil), "myapp_session", "tok"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — an expired session must never authenticate", resp.StatusCode)
	}
}

// The absolute cap is the real bound on how long a revoked identity keeps
// access, because Owner and Groups are frozen at login and never re-derived.
func TestSessionOverTheAbsoluteCapIsRejectedHoweverActive(t *testing.T) {
	now := time.Now().UTC()
	store := newMemStore()
	s := liveSession(true)
	s.CreatedAt = now.Add(-40 * 24 * time.Hour) // older than DefaultPolicy's 30-day cap
	s.ExpiresAt = now.Add(time.Hour)            // …but freshly renewed
	store.put("tok", s)

	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)
	app := probeApp(g)

	resp := do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/admin", nil), "myapp_session", "tok"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — the absolute cap must beat a sliding renewal", resp.StatusCode)
	}
}

// A renewal must never push expiry past the cap: an unclamped row is rejected at
// read time but never *expired*, so a sweeper can never collect it.
func TestTouchClampsRenewalToTheAbsoluteCap(t *testing.T) {
	now := time.Now().UTC()
	store := newMemStore()
	s := liveSession(true)
	s.CreatedAt = now.Add(-29 * 24 * time.Hour) // one day left under the 30-day cap
	s.LastSeenAt = now.Add(-2 * time.Hour)      // stale enough to be touched
	s.ExpiresAt = now.Add(time.Hour)
	store.put("tok", s)

	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)
	app := probeApp(g)
	do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "tok"))

	store.mu.Lock()
	got, touched := store.touched["sess-1"]
	store.mu.Unlock()
	if !touched {
		t.Fatal("a stale session must be touched")
	}
	hard := s.CreatedAt.Add(oidcauth.DefaultPolicy().MaxLifetime)
	if got.After(hard) {
		t.Errorf("renewed to %v, past the hard cap %v — the row would become uncollectable", got, hard)
	}
}

// A session seen moments ago must not cause a write: without the interval every
// authenticated request would write to the database.
func TestFreshSessionIsNotTouched(t *testing.T) {
	store := newMemStore()
	store.put("tok", liveSession(true)) // LastSeenAt == now
	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)
	app := probeApp(g)

	do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "tok"))

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.touched) != 0 {
		t.Error("a just-seen session must not be written back on every request")
	}
}

// ── Cookie naming: the __Host- prefix ───────────────────────────────────────

// The prefix is what stops a sibling subdomain — or an on-path attacker forging
// a plain-HTTP response — from planting a cookie this server would read as its
// own. On HTTPS the prefixed name is the only one read.
func TestHTTPSDeploymentUsesAndOnlyReadsThePrefixedCookie(t *testing.T) {
	store := newMemStore()
	store.put("tok", liveSession(true))
	g := newGate(t, newTestApp(t), testProvider(t, "https://app.example.com"), store)
	app := probeApp(g)

	t.Run("prefixed name is read", func(t *testing.T) {
		resp := do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "__Host-myapp_session", "tok"))
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "id:u-1" {
			t.Errorf("body = %q, want id:u-1", body)
		}
	})

	// Deliberately NOT dual-read: accepting the unprefixed name as a fallback
	// hands the attacker back exactly the cookie they were going to plant, which
	// is the whole attack.
	t.Run("unprefixed name is ignored on HTTPS", func(t *testing.T) {
		resp := do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "tok"))
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "anon" {
			t.Errorf("body = %q, want anon — reading the unprefixed name reopens login CSRF", body)
		}
	})
}

// A __Host- Set-Cookie must carry Secure, Path=/ and no Domain, or the browser
// rejects the whole header — which would make a clear silently a no-op on
// exactly the deployments that use the prefix.
func TestClearingAPrefixedCookieKeepsTheAttributesTheBrowserDemands(t *testing.T) {
	a := newTestApp(t)
	g := newGate(t, a, nil, newMemStore())
	app := a.HTTP

	resp := do(t, app, httptest.NewRequest(http.MethodPost, g.LogoutPath(), nil))
	for _, c := range resp.Cookies() {
		if !strings.HasPrefix(c.Name, "__Host-") {
			continue
		}
		if !c.Secure {
			t.Errorf("%s cleared without Secure — the browser rejects the header", c.Name)
		}
		if c.Path != "/" {
			t.Errorf("%s cleared with Path=%q, want /", c.Name, c.Path)
		}
		if c.Domain != "" {
			t.Errorf("%s cleared with Domain=%q, want none", c.Name, c.Domain)
		}
	}
}

// ── Open redirect ───────────────────────────────────────────────────────────

// The cases that matter are the ones that look like paths but are not.
func TestSafeReturnPathRefusesOffSiteDestinations(t *testing.T) {
	offSite := []string{
		"//evil.example",              // protocol-relative: another origin behind a leading slash
		"///evil.example",             //
		"/\\evil.example",             // a backslash browsers normalise to a slash
		"\\\\evil.example",            //
		"https://evil.example",        // an outright absolute URL
		"http://evil.example/path",    //
		"javascript:alert(1)",         // a scheme that is not even a location
		"/path\nSet-Cookie: x=y",      // a control character
		"/path\r\nLocation: /evil",    //
		strings.Repeat("/a", 400),     // longer than a browser will store in the cookie
		"//evil.example/\\@good.test", //
	}
	for _, raw := range offSite {
		if got := SafeReturnPath(raw); got != "/" {
			t.Errorf("SafeReturnPath(%q) = %q, want / — this is an open redirect", raw, got)
		}
	}
}

func TestSafeReturnPathKeepsRealSamesitePaths(t *testing.T) {
	for _, raw := range []string{"/", "/dashboard", "/a/b/c?q=1&r=2", "/path#frag"} {
		if got := SafeReturnPath(raw); got != raw {
			t.Errorf("SafeReturnPath(%q) = %q, want it unchanged", raw, got)
		}
	}
}

// ── The handshake cookie ────────────────────────────────────────────────────

// A blank nonce sails through the exchange's own comparison as "" != "" against
// a token that carries no nonce claim — silently disabling the replay check. The
// cookie parser is the caller-side half of that defence.
func TestParseStateCookieRefusesAnythingIncomplete(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":          "",
		"too few fields": "state|nonce|verifier",
		"too many":       "s|n|v|next|extra",
		"blank state":    "|n|v|",
		"blank nonce":    "s||v|",
		"blank verifier": "s|n||",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := parseStateCookie(raw); ok {
				t.Errorf("parseStateCookie(%q) accepted an unusable handshake", raw)
			}
		})
	}
}

// The return path spent the handshake in a cookie, and a cookie is client state:
// trusting the check made on the way in means trusting the browser to have
// preserved it.
func TestParseStateCookieRevalidatesTheReturnPathOnTheWayOut(t *testing.T) {
	// A hostile value base64url-encoded into the fourth field, as a tampered
	// cookie would carry.
	tampered := "s|n|v|" + base64Raw("//evil.example")
	_, next, ok := parseStateCookie(tampered)
	if !ok {
		t.Fatal("the handshake itself is well-formed")
	}
	if next != "/" {
		t.Errorf("next = %q, want / — the return path must be re-validated on the way out", next)
	}
}

// An empty fourth field is normal: no destination simply means "/".
func TestParseStateCookieAcceptsAnEmptyReturnPath(t *testing.T) {
	hs, next, ok := parseStateCookie("s|n|v|")
	if !ok || next != "/" {
		t.Errorf("parseStateCookie = (%+v, %q, %v), want a valid handshake returning /", hs, next, ok)
	}
}

func base64Raw(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	b := []byte(s)
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint(chunk[0])<<16 | uint(chunk[1])<<8 | uint(chunk[2])
		for j := 0; j < n+1; j++ {
			out = append(out, alphabet[(v>>uint(18-6*j))&0x3f])
		}
	}
	return string(out)
}

// ── Provider-controlled input in logs ───────────────────────────────────────

// error_description is provider-controlled free text arriving in a query string:
// a newline would let it forge additional log entries.
func TestBoundedDescriptionCollapsesNewlinesAndBounds(t *testing.T) {
	got := boundedDescription("line one\nlevel=error msg=\"forged\"\r\n\ttab")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("boundedDescription kept a control character: %q", got)
	}
	if long := boundedDescription(strings.Repeat("x", 500)); len(long) != 200 {
		t.Errorf("length = %d, want it bounded to 200", len(long))
	}
}

// ── Whoami ──────────────────────────────────────────────────────────────────

// LoginURL must always be present: the sign-in screen needs somewhere to point
// precisely when nobody is signed in, which is when it is rendered.
func TestWhoamiAlwaysCarriesALoginURL(t *testing.T) {
	a := newTestApp(t)
	g := newGate(t, a, testProvider(t, "http://app.test"), newMemStore())
	app := a.HTTP

	resp := do(t, app, httptest.NewRequest(http.MethodGet, g.WhoamiPath(), nil))
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"authenticated":false`, `"ssoEnabled":true`, `"loginUrl":"/api/auth/login"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("whoami body %s missing %s", body, want)
		}
	}
}

// An authenticated non-owner must serialize "owner": false. A plain bool with
// omitempty drops exactly that case, leaving a client unable to tell "not the
// owner" from "no identity" — which is why Owner is a pointer.
func TestWhoamiDistinguishesNonOwnerFromAnonymous(t *testing.T) {
	store := newMemStore()
	store.put("member", liveSession(false))
	a := newTestApp(t)
	g := newGate(t, a, testProvider(t, "http://app.test"), store)
	app := a.HTTP

	resp := do(t, app, withCookie(httptest.NewRequest(http.MethodGet, g.WhoamiPath(), nil), "myapp_session", "member"))
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"owner":false`) {
		t.Errorf("body = %s, want an explicit \"owner\":false", body)
	}
}

// ── Session administration ──────────────────────────────────────────────────

// Nothing about the credential may leave the database, not even in a form only
// an owner can read.
func TestSessionListNeverExposesTheTokenHash(t *testing.T) {
	store := newMemStore()
	token := store.put("owner-token", liveSession(true))
	a := newTestApp(t)
	g := newGate(t, a, testProvider(t, "http://app.test"), store)
	app := a.HTTP

	resp := do(t, app, withCookie(httptest.NewRequest(http.MethodGet, g.SessionsPath(), nil), "myapp_session", token))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), oidcauth.HashToken(token)) || strings.Contains(string(body), token) {
		t.Errorf("session list leaked the credential: %s", body)
	}
	// The caller's own row is marked so a client can label it and refuse to
	// revoke it by accident.
	if !strings.Contains(string(body), `"current":true`) {
		t.Errorf("body = %s, want the caller's own row marked current", body)
	}
}

// Revoking an already-gone session still succeeds: the caller's intent is
// satisfied either way, and a 404 would leak which session ids exist.
func TestRevokingAnUnknownSessionSucceeds(t *testing.T) {
	store := newMemStore()
	token := store.put("owner-token", liveSession(true))
	a := newTestApp(t)
	g := newGate(t, a, testProvider(t, "http://app.test"), store)
	app := a.HTTP

	resp := do(t, app, withCookie(
		httptest.NewRequest(http.MethodDelete, g.SessionsPath()+"/no-such-session", nil),
		"myapp_session", token))
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

// ── Route agreement ─────────────────────────────────────────────────────────

// The gate's callback route and the provider's registered redirect URI must
// agree, or every sign-in 404s after a round trip to the provider. They are
// configured in two places, so nothing but a test holds them together.
func TestCallbackPathMatchesTheProviderRedirectURI(t *testing.T) {
	p := testProvider(t, "https://app.example.com")
	g := newGate(t, newTestApp(t), p, newMemStore())
	if !strings.HasSuffix(p.RedirectURI(), g.CallbackPath()) {
		t.Errorf("provider redirect %q does not end in the gate's callback path %q",
			p.RedirectURI(), g.CallbackPath())
	}
}

// ── Concurrency ─────────────────────────────────────────────────────────────

// Identity resolution runs on every request; it must be race-free under -race.
func TestResolveIdentityIsRaceFree(t *testing.T) {
	store := newMemStore()
	store.put("tok", liveSession(true))
	g := newGate(t, newTestApp(t), testProvider(t, "http://app.test"), store)
	app := probeApp(g)

	var wg sync.WaitGroup
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(t, app, withCookie(httptest.NewRequest(http.MethodGet, "/open", nil), "myapp_session", "tok"))
		}()
	}
	wg.Wait()
}

// ── New wires the gate ──────────────────────────────────────────────────────

// One call builds the gate and puts it in the boot report, so a caller never
// writes Declare for login.
func TestNewDeclaresTheLoginComponent(t *testing.T) {
	a := newTestApp(t)
	newGate(t, a, nil, newMemStore())

	c, ok := componentNamed(a, "login")
	if !ok {
		t.Fatalf("login missing from Components(): %+v", a.Components())
	}
	if c.On {
		t.Error("a gate with no Provider must be declared off")
	}
	if c.Detail == "" {
		t.Error("an off gate must say why in Detail")
	}
}

// A configured gate must name its issuer in the report, so an operator can see
// which identity provider this process actually trusts.
func TestNewDeclaresAnEnabledGateWithItsIssuer(t *testing.T) {
	a := newTestApp(t)
	newGate(t, a, testProvider(t, "https://app.example.com"), newMemStore())

	c, ok := componentNamed(a, "login")
	if !ok {
		t.Fatalf("login missing from Components(): %+v", a.Components())
	}
	if !c.On {
		t.Error("a gate with a Provider must be declared on")
	}
	if c.Detail != "https://id.example.com" {
		t.Errorf("Detail = %q, want the issuer", c.Detail)
	}
}

// The handshake routes are mounted by New, not by a separate Register call that
// a caller can forget.
func TestNewMountsTheHandshakeRoutes(t *testing.T) {
	a := newTestApp(t)
	g := newGate(t, a, testProvider(t, "http://app.test"), newMemStore())

	resp := do(t, a.HTTP, httptest.NewRequest(http.MethodGet, g.LoginPath(), nil))
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("%s returned 404 — New did not mount the handshake routes", g.LoginPath())
	}
}

// The ordering New exists to guarantee. Whoami reads the identity that
// ResolveIdentity resolves, so the middleware has to be mounted ahead of the
// route. Registering them the other way round makes whoami report anonymous for
// a signed-in user — silently, and with every other test still passing, because
// a caller who mounts its own middleware first would never see it.
func TestNewMountsResolveIdentityAheadOfTheRoutes(t *testing.T) {
	store := newMemStore()
	token := store.put("owner-token", liveSession(true))
	a := newTestApp(t)
	g := newGate(t, a, testProvider(t, "http://app.test"), store)

	resp := do(t, a.HTTP, withCookie(
		httptest.NewRequest(http.MethodGet, g.WhoamiPath(), nil), "myapp_session", token))
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "u-1") {
		t.Errorf("whoami = %s\nwant the resolved subject — ResolveIdentity did not run before the route", body)
	}
}
