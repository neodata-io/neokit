package fiberauth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	_ "modernc.org/sqlite" // the driver the token store is opened with

	"github.com/neodata-io/neokit/apitoken"
	neoapp "github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/oidcauth"
)

// ── Rig ─────────────────────────────────────────────────────────────────────

// spyTokens is the real SQLite store, counting what the middleware asks of it
// and failing on demand.
type spyTokens struct {
	apitoken.Store
	lookups, touches atomic.Int32
	failLookup       error
}

func (s *spyTokens) TokenByHash(ctx context.Context, hash string) (apitoken.Token, bool, error) {
	s.lookups.Add(1)
	if s.failLookup != nil {
		return apitoken.Token{}, false, s.failLookup
	}
	return s.Store.TokenByHash(ctx, hash)
}

func (s *spyTokens) TouchToken(ctx context.Context, id string, at time.Time) error {
	s.touches.Add(1)
	return s.Store.TouchToken(ctx, id, at)
}

type tokenRig struct {
	t      *testing.T
	a      *neoapp.App
	g      *Gate
	tokens *spyTokens
}

const (
	memberCookie = "member-session"
	ownerCookie  = "owner-session"
)

// newTokenRig builds an app with a login, two signed-in browsers (a member and
// an owner) and personal API tokens on. provider may be swapped per test through
// rig.g's Options.Provider closure by passing a pointer the test controls.
func newTokenRig(t *testing.T, provider **oidcauth.Provider, adjust ...func(*Options)) *tokenRig {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	real, err := apitoken.NewSQLite(db, apitoken.SQLiteOptions{MaxPerSubject: 3})
	if err != nil {
		t.Fatal(err)
	}
	tokens := &spyTokens{Store: real}

	// Sessions that outlive the clock some tests move forward by a day or two.
	week := time.Now().UTC().Add(7 * 24 * time.Hour)
	sessions := newMemStore()
	member := liveSession(false)
	member.ExpiresAt = week
	sessions.put(memberCookie, member)
	owner := liveSession(true)
	owner.ExpiresAt = week
	owner.ID, owner.Subject, owner.Name, owner.Email = "sess-owner", "u-9", "Robin", "robin@example.com"
	sessions.put(ownerCookie, owner)

	if provider == nil {
		p := testProvider(t, "http://app.test")
		provider = &p
	}
	o := Options{
		Provider:     func() *oidcauth.Provider { return *provider },
		Sessions:     sessions,
		CookiePrefix: "myapp",
		RateLimit:    -1,
		Tokens:       tokens,
	}
	for _, f := range adjust {
		f(&o)
	}
	a := newTestApp(t)
	g := New(a, o)

	who := func(c fiber.Ctx) error {
		id, ok := IdentityFrom(c)
		if !ok {
			return c.SendString("anon")
		}
		via := "session"
		if ViaAPIToken(c) {
			via = "token"
		}
		return c.SendString(id.Subject + " " + via)
	}
	a.API.Get("/open", who)
	a.API.Get("/member", g.RequireAuth(), who)
	a.API.Get("/admin", g.RequireOwner(), who)
	a.API.Post("/secrets", RefuseAPIToken, g.RequireOwner(), who)
	return &tokenRig{t: t, a: a, g: g, tokens: tokens}
}

// call sends one request. cookie and bearer are each optional.
func (r *tokenRig) call(method, path, cookie, bearer string, body any) *http.Response {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		withCookie(req, "myapp_session", cookie)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return do(r.t, r.a.HTTP, req)
}

func text(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	raw := text(t, resp)
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return v
}

// mint makes a token from a browser session and returns it.
func (r *tokenRig) mint(cookie, name string) CreatedTokenView {
	r.t.Helper()
	resp := r.call(http.MethodPost, r.g.MyTokensPath(), cookie, "", map[string]any{"name": name})
	if resp.StatusCode != http.StatusCreated {
		r.t.Fatalf("mint: status = %d, body = %s", resp.StatusCode, text(r.t, resp))
	}
	return decode[CreatedTokenView](r.t, resp)
}

func (r *tokenRig) open(bearer string) string {
	r.t.Helper()
	return text(r.t, r.call(http.MethodGet, r.a.APIBase+"/open", "", bearer, nil))
}

// ── Recognising a token ─────────────────────────────────────────────────────

func TestATokenActsAsItsMaker(t *testing.T) {
	r := newTokenRig(t, nil)
	tok := r.mint(memberCookie, "ci")

	if got := r.open(tok.Token); got != "u-1 token" {
		t.Errorf("open route saw %q, want the maker, by token", got)
	}
	if resp := r.call(http.MethodGet, r.a.APIBase+"/member", "", tok.Token, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("member route: status = %d, want 200", resp.StatusCode)
	}
	// A member's token is a member: it grants what its maker has and no more.
	if resp := r.call(http.MethodGet, r.a.APIBase+"/admin", "", tok.Token, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("admin route with a member's token: status = %d, want 403", resp.StatusCode)
	}
	owner := r.mint(ownerCookie, "ops")
	if resp := r.call(http.MethodGet, r.a.APIBase+"/admin", "", owner.Token, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("admin route with an owner's token: status = %d, want 200", resp.StatusCode)
	}
}

func TestTheTokenCarriesThePrefixAndIsShownOnce(t *testing.T) {
	r := newTokenRig(t, nil)
	resp := r.call(http.MethodPost, r.g.MyTokensPath(), memberCookie, "", map[string]any{"name": "  laptop  "})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q: a response holding a credential must not be cached", got)
	}
	made := decode[CreatedTokenView](t, resp)
	if want := r.g.MyTokensPath() + "/" + made.ID; resp.Header.Get("Location") != want {
		t.Errorf("Location = %q, want %q", resp.Header.Get("Location"), want)
	}
	// The default prefix is the cookie prefix, so one setting names both.
	if !strings.HasPrefix(made.Token, "myapp_") {
		t.Errorf("token %q does not carry the default prefix", made.Token)
	}
	if made.Name != "laptop" {
		t.Errorf("name = %q, want it trimmed", made.Name)
	}

	// The list never shows it again — nor anything it could be recovered from.
	list := text(t, r.call(http.MethodGet, r.g.MyTokensPath(), memberCookie, "", nil))
	if strings.Contains(list, made.Token) || strings.Contains(list, apitoken.Hash(made.Token)) || strings.Contains(list, `"token"`) {
		t.Errorf("the list leaks the credential: %s", list)
	}
}

// With no login there is no "you" to be: the middleware reads nothing, and the
// routes are not there.
func TestTokensAreInertWithoutALogin(t *testing.T) {
	var none *oidcauth.Provider
	r := newTokenRig(t, &none)

	if got := r.open("myapp_anything"); got != "anon" {
		t.Errorf("open route = %q, want anon", got)
	}
	if n := r.tokens.lookups.Load(); n != 0 {
		t.Errorf("the store was asked %d times with no login configured", n)
	}
	for _, path := range []string{r.g.MyTokensPath(), r.g.TokensPath()} {
		if resp := r.call(http.MethodGet, path, "", "", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, resp.StatusCode)
		}
	}
	if resp := r.call(http.MethodDelete, r.g.CurrentTokenPath(), "", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE current token: status = %d, want 404", resp.StatusCode)
	}
}

// A request without a token must cost one header read — and a bearer value that
// is not this application's is somebody else's business.
func TestOtherBearerValuesAreLeftAlone(t *testing.T) {
	r := newTokenRig(t, nil)
	for _, bearer := range []string{"", "eyJhbGciOi.some.jwt", "other_abc"} {
		if got := r.open(bearer); got != "anon" {
			t.Errorf("bearer %q: open route = %q, want anon", bearer, got)
		}
	}
	if n := r.tokens.lookups.Load(); n != 0 {
		t.Errorf("the store was asked %d times for values that are not ours", n)
	}
}

// A token that cannot be matched is refused outright — on an open route, and
// with a perfectly good session cookie alongside. Falling through would have a
// script with a revoked token carry on as somebody else.
func TestAnUnmatchedTokenIsRefusedWhateverElseTheRequestCarries(t *testing.T) {
	r := newTokenRig(t, nil)

	resp := r.call(http.MethodGet, r.a.APIBase+"/open", "", "myapp_never-issued", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("open route: status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}

	resp = r.call(http.MethodGet, r.a.APIBase+"/member", memberCookie, "myapp_never-issued", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("with a live session alongside: status = %d, want 401", resp.StatusCode)
	}
}

func TestARevokedTokenStopsAtOnce(t *testing.T) {
	r := newTokenRig(t, nil)
	tok := r.mint(memberCookie, "ci")

	if resp := r.call(http.MethodDelete, r.g.MyTokensPath()+"/"+tok.ID, memberCookie, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status = %d", resp.StatusCode)
	}
	if resp := r.call(http.MethodGet, r.a.APIBase+"/open", "", tok.Token, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after revoking: status = %d, want 401", resp.StatusCode)
	}
}

// A token belongs to the login it was made under. Point the application at
// another identity provider and every token is retired by that alone.
func TestATokenFromAnotherLoginIsRefused(t *testing.T) {
	p := testProvider(t, "http://app.test")
	r := newTokenRig(t, &p)
	tok := r.mint(memberCookie, "ci")

	other, ok := oidcauth.New(oidcauth.Config{
		Issuer: "https://other-idp.example.com", ClientID: "app", ClientSecret: "s",
		BaseURL: "http://app.test", OwnerGroup: "admins",
	})
	if !ok {
		t.Fatal("New declined a complete config")
	}
	p = other

	if resp := r.call(http.MethodGet, r.a.APIBase+"/open", "", tok.Token, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 under the new login", resp.StatusCode)
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	r := newTokenRig(t, nil)
	resp := r.call(http.MethodPost, r.g.MyTokensPath(), memberCookie, "", map[string]any{"name": "ci", "expiresInDays": 1})
	tok := decode[CreatedTokenView](t, resp)
	if tok.ExpiresAt == nil {
		t.Fatal("expiresInDays produced a token with no expiry")
	}
	if got := r.open(tok.Token); got != "u-1 token" {
		t.Fatalf("before expiry: %q", got)
	}

	r.g.now = func() time.Time { return time.Now().Add(24*time.Hour + time.Minute) }
	if resp := r.call(http.MethodGet, r.a.APIBase+"/open", "", tok.Token, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after expiry: status = %d, want 401", resp.StatusCode)
	}
}

// Not knowing is not "anonymous": a store that cannot be asked must not let the
// request through as nobody.
func TestAStoreFailureIsNotAnonymous(t *testing.T) {
	r := newTokenRig(t, nil)
	tok := r.mint(memberCookie, "ci")
	r.tokens.failLookup = errors.New("disk on fire")

	resp := r.call(http.MethodGet, r.a.APIBase+"/open", "", tok.Token, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if body := text(t, resp); strings.Contains(body, "disk on fire") {
		t.Errorf("the cause travelled to the client: %s", body)
	}
}

// Ownership follows the owner group in force today, as it does for a session.
func TestATokensOwnershipFollowsTheCurrentOwnerGroup(t *testing.T) {
	p := testProvider(t, "http://app.test")
	r := newTokenRig(t, &p)
	tok := r.mint(ownerCookie, "ops")

	tightened, _ := oidcauth.New(oidcauth.Config{
		Issuer: "https://id.example.com", ClientID: "app", ClientSecret: "s",
		BaseURL: "http://app.test", OwnerGroup: "superadmins",
	})
	p = tightened

	if resp := r.call(http.MethodGet, r.a.APIBase+"/admin", "", tok.Token, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 once the owner group no longer includes the maker", resp.StatusCode)
	}
}

// A script polling every second must not turn every request into a write.
func TestLastUseIsWrittenAtMostOncePerInterval(t *testing.T) {
	r := newTokenRig(t, nil)
	tok := r.mint(memberCookie, "ci")

	for range 5 {
		r.open(tok.Token)
	}
	if n := r.tokens.touches.Load(); n != 1 {
		t.Errorf("5 requests wrote the last use %d times, want 1", n)
	}

	r.g.now = func() time.Time { return time.Now().Add(apitoken.DefaultTouchInterval + time.Minute) }
	r.open(tok.Token)
	if n := r.tokens.touches.Load(); n != 2 {
		t.Errorf("a use after the interval: %d writes in total, want 2", n)
	}
}

// The gate's own routes answer for the cookie alone: the owner-only session
// list holds every browser's name and email, and is not a token's to read.
func TestTheGatesOwnRoutesDoNotSeeAToken(t *testing.T) {
	r := newTokenRig(t, nil)
	owner := r.mint(ownerCookie, "ops")

	who := decode[WhoamiView](t, r.call(http.MethodGet, r.g.WhoamiPath(), "", owner.Token, nil))
	if who.Authenticated {
		t.Error("whoami recognised a token")
	}
	if resp := r.call(http.MethodGet, r.g.SessionsPath(), "", owner.Token, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("session list with an owner's token: status = %d, want 401", resp.StatusCode)
	}
}

// ── What a token may not do ─────────────────────────────────────────────────

// A leaked token must not be able to make more tokens, or see the ones there
// are. The answer names the reason, so it does not read as a wrong credential.
func TestATokenCannotManageTokens(t *testing.T) {
	r := newTokenRig(t, nil)
	owner := r.mint(ownerCookie, "ops")

	cases := []struct{ method, path string }{
		{http.MethodGet, r.g.MyTokensPath()},
		{http.MethodPost, r.g.MyTokensPath()},
		{http.MethodDelete, r.g.MyTokensPath() + "/" + owner.ID},
		{http.MethodGet, r.g.TokensPath()},
		{http.MethodDelete, r.g.TokensPath() + "/" + owner.ID},
	}
	for _, tc := range cases {
		resp := r.call(tc.method, tc.path, "", owner.Token, map[string]any{"name": "more"})
		body := text(t, resp)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"code":"token_not_allowed"`) {
			t.Errorf("%s %s: status = %d, body = %s; want 403 token_not_allowed", tc.method, tc.path, resp.StatusCode, body)
		}
	}
	// Nothing was made or removed along the way.
	if all, _ := r.tokens.ListAllTokens(t.Context()); len(all) != 1 {
		t.Errorf("%d tokens exist, want the 1 the owner made", len(all))
	}
}

// RefuseAPIToken is the guard an application puts on its own sensitive routes.
func TestRefuseAPITokenGuardsAnApplicationsOwnRoutes(t *testing.T) {
	r := newTokenRig(t, nil)
	owner := r.mint(ownerCookie, "ops")

	if resp := r.call(http.MethodPost, r.a.APIBase+"/secrets", "", owner.Token, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("with the owner's token: status = %d, want 403", resp.StatusCode)
	}
	if resp := r.call(http.MethodPost, r.a.APIBase+"/secrets", ownerCookie, "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("with the owner's session: status = %d, want 200", resp.StatusCode)
	}
}

func TestMakingATokenNeedsSomeoneSignedIn(t *testing.T) {
	r := newTokenRig(t, nil)
	if resp := r.call(http.MethodPost, r.g.MyTokensPath(), "", "", map[string]any{"name": "x"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", resp.StatusCode)
	}
}

func TestATokenNeedsAName(t *testing.T) {
	r := newTokenRig(t, nil)
	for name, body := range map[string]any{
		"blank":     map[string]any{"name": "   "},
		"missing":   map[string]any{},
		"too long":  map[string]any{"name": strings.Repeat("n", apitoken.NameMaxLen+1)},
		"zero days": map[string]any{"name": "ok", "expiresInDays": 0},
	} {
		resp := r.call(http.MethodPost, r.g.MyTokensPath(), memberCookie, "", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, resp.StatusCode)
		}
	}
}

func TestTheLimitAnswersAConflictThatNamesItself(t *testing.T) {
	r := newTokenRig(t, nil) // the rig's store allows 3
	for _, n := range []string{"a", "b", "c"} {
		r.mint(memberCookie, n)
	}
	resp := r.call(http.MethodPost, r.g.MyTokensPath(), memberCookie, "", map[string]any{"name": "d"})
	body := text(t, resp)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, `"code":"too_many_tokens"`) {
		t.Errorf("status = %d, body = %s; want 409 too_many_tokens", resp.StatusCode, body)
	}
}

// ── Lifetime ────────────────────────────────────────────────────────────────

func TestTokenLifetimeIsWhatWasAskedNeverPastTheMaximum(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name   string
		maxAge time.Duration
		ask    any // expiresInDays, or nil
		want   time.Duration
	}{
		{"nothing asked, no maximum", -1, nil, 0},
		{"asked, no maximum", -1, 30, 30 * day},
		{"nothing asked, a maximum", 90 * day, nil, 90 * day},
		{"asked for less than the maximum", 90 * day, 7, 7 * day},
		{"asked for more than the maximum", 90 * day, 365, 90 * day},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTokenRig(t, nil, func(o *Options) { o.TokenMaxAge = tc.maxAge })
			body := map[string]any{"name": "ci"}
			if tc.ask != nil {
				body["expiresInDays"] = tc.ask
			}
			made := decode[CreatedTokenView](t, r.call(http.MethodPost, r.g.MyTokensPath(), memberCookie, "", body))

			if tc.want == 0 {
				if made.ExpiresAt != nil {
					t.Fatalf("expiresAt = %s, want none", *made.ExpiresAt)
				}
				return
			}
			if made.ExpiresAt == nil {
				t.Fatal("expiresAt is missing")
			}
			created, _ := time.Parse(time.RFC3339, made.CreatedAt)
			expires, _ := time.Parse(time.RFC3339, *made.ExpiresAt)
			if got := expires.Sub(created); got != tc.want {
				t.Errorf("lifetime = %s, want %s", got, tc.want)
			}
		})
	}
}

// ── Managing tokens ─────────────────────────────────────────────────────────

func TestAPersonSeesAndRevokesOnlyTheirOwn(t *testing.T) {
	r := newTokenRig(t, nil)
	mine := r.mint(memberCookie, "mine")
	theirs := r.mint(ownerCookie, "theirs")

	list := decode[[]TokenView](t, r.call(http.MethodGet, r.g.MyTokensPath(), memberCookie, "", nil))
	if len(list) != 1 || list[0].ID != mine.ID {
		t.Errorf("list = %+v, want only my own", list)
	}

	// Someone else's answers exactly like one that does not exist.
	for _, id := range []string{theirs.ID, "no-such-id"} {
		if resp := r.call(http.MethodDelete, r.g.MyTokensPath()+"/"+id, memberCookie, "", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("DELETE %s: status = %d, want 404", id, resp.StatusCode)
		}
	}
	if got := r.open(theirs.Token); got != "u-9 token" {
		t.Errorf("the other person's token stopped working: %q", got)
	}
}

func TestAnEmptyListIsAnEmptyArray(t *testing.T) {
	r := newTokenRig(t, nil)
	if got := strings.TrimSpace(text(t, r.call(http.MethodGet, r.g.MyTokensPath(), memberCookie, "", nil))); got != "[]" {
		t.Errorf("body = %s, want []", got)
	}
}

// How an app signs out: it revokes the token it holds, and nothing else.
func TestATokenCanRevokeItself(t *testing.T) {
	r := newTokenRig(t, nil)
	tok := r.mint(memberCookie, "phone")
	other := r.mint(memberCookie, "laptop")

	if resp := r.call(http.MethodDelete, r.g.CurrentTokenPath(), "", tok.Token, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if resp := r.call(http.MethodGet, r.a.APIBase+"/open", "", tok.Token, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked token still works: status = %d", resp.StatusCode)
	}
	if got := r.open(other.Token); got != "u-1 token" {
		t.Errorf("the person's other token went with it: %q", got)
	}
	// A browser has no token to revoke; its way out is logout.
	if resp := r.call(http.MethodDelete, r.g.CurrentTokenPath(), memberCookie, "", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("from a session: status = %d, want 400", resp.StatusCode)
	}
}

func TestTheOwnerSeesAndRevokesEveryones(t *testing.T) {
	r := newTokenRig(t, nil)
	members := r.mint(memberCookie, "ci")
	r.mint(ownerCookie, "ops")

	if resp := r.call(http.MethodGet, r.g.TokensPath(), memberCookie, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a member reading everyone's tokens: status = %d, want 403", resp.StatusCode)
	}
	if resp := r.call(http.MethodGet, r.g.TokensPath(), "", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", resp.StatusCode)
	}

	all := decode[[]OwnedTokenView](t, r.call(http.MethodGet, r.g.TokensPath(), ownerCookie, "", nil))
	if len(all) != 2 {
		t.Fatalf("the owner sees %d tokens, want 2", len(all))
	}
	for _, v := range all {
		if !v.Active || v.Subject == "" || v.Person == "" {
			t.Errorf("view = %+v, want an active token that says whose it is", v)
		}
	}

	if resp := r.call(http.MethodDelete, r.g.TokensPath()+"/"+members.ID, ownerCookie, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner revoking a member's token: status = %d", resp.StatusCode)
	}
	if resp := r.call(http.MethodGet, r.a.APIBase+"/open", "", members.Token, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the revoked token still works: status = %d", resp.StatusCode)
	}
	if resp := r.call(http.MethodDelete, r.g.TokensPath()+"/"+members.ID, ownerCookie, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoking it again: status = %d, want 404", resp.StatusCode)
	}
}

// The overview must never call a token active that a request would be refused
// with: it applies the middleware's own two checks.
func TestTheOverviewMarksDeadTokens(t *testing.T) {
	p := testProvider(t, "http://app.test")
	r := newTokenRig(t, &p)
	r.call(http.MethodPost, r.g.MyTokensPath(), memberCookie, "", map[string]any{"name": "short", "expiresInDays": 1})
	r.mint(memberCookie, "long")

	active := func() map[string]bool {
		out := map[string]bool{}
		for _, v := range decode[[]OwnedTokenView](t, r.call(http.MethodGet, r.g.TokensPath(), ownerCookie, "", nil)) {
			out[v.Name] = v.Active
		}
		return out
	}
	if got := active(); !got["short"] || !got["long"] {
		t.Fatalf("freshly made: active = %v, want both", got)
	}

	// Two days on, the one-day token has expired.
	r.g.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if got := active(); got["short"] || !got["long"] {
		t.Errorf("after two days: active = %v, want only long", got)
	}

	// And under another login, none of them count.
	other, _ := oidcauth.New(oidcauth.Config{
		Issuer: "https://other-idp.example.com", ClientID: "app", ClientSecret: "s",
		BaseURL: "http://app.test", OwnerGroup: "admins",
	})
	p = other
	if got := active(); got["short"] || got["long"] {
		t.Errorf("under another login: active = %v, want none", got)
	}
}

// ── Off by default ──────────────────────────────────────────────────────────

// Without Options.Tokens nothing is mounted: no routes, and a bearer value is
// not looked at.
func TestWithoutAStoreNothingIsMounted(t *testing.T) {
	a := newTestApp(t)
	sessions := newMemStore()
	sessions.put(memberCookie, liveSession(false))
	g := newGate(t, a, testProvider(t, "http://app.test"), sessions)
	a.API.Get("/open", func(c fiber.Ctx) error { return c.SendString("ok") })

	req := withCookie(httptest.NewRequest(http.MethodGet, g.MyTokensPath(), nil), "myapp_session", memberCookie)
	if resp := do(t, a.HTTP, req); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET my tokens: status = %d, want 404", resp.StatusCode)
	}
	req = httptest.NewRequest(http.MethodGet, a.APIBase+"/open", nil)
	req.Header.Set("Authorization", "Bearer myapp_anything")
	if resp := do(t, a.HTTP, req); resp.StatusCode != http.StatusOK {
		t.Errorf("a bearer value with tokens off: status = %d, want 200", resp.StatusCode)
	}
}

// ── Without the routes ──────────────────────────────────────────────────────

// ownRig is an application that manages tokens through routes of its own: a
// gate without Options.Tokens, and APITokenMiddleware mounted by hand after it.
type ownRig struct {
	t      *testing.T
	a      *neoapp.App
	g      *Gate
	tokens *spyTokens
	now    time.Time
}

func newOwnRig(t *testing.T, provider **oidcauth.Provider) *ownRig {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	real, err := apitoken.NewSQLite(db, apitoken.SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tokens := &spyTokens{Store: real}

	if provider == nil {
		p := testProvider(t, "http://app.test")
		provider = &p
	}
	resolve := func() *oidcauth.Provider { return *provider }
	sessions := newMemStore()
	sessions.put(ownerCookie, liveSession(true))

	a := newTestApp(t)
	g := New(a, Options{Provider: resolve, Sessions: sessions, CookiePrefix: "myapp", RateLimit: -1})
	a.HTTP.Use(APITokenMiddleware(resolve, tokens, "own_"))
	a.API.Get("/open", func(c fiber.Ctx) error {
		id, ok := IdentityFrom(c)
		if !ok {
			return c.SendString("anon")
		}
		via := "session"
		if ViaAPIToken(c) {
			via = "token"
		}
		return c.SendString(id.Subject + " " + via)
	})
	a.API.Post("/secrets", RefuseAPIToken, g.RequireOwner(), func(c fiber.Ctx) error { return c.SendString("in") })
	return &ownRig{t: t, a: a, g: g, tokens: tokens, now: time.Now().UTC().Truncate(time.Second)}
}

// mint stores a token the way an application's own route would, and returns
// the secret.
func (r *ownRig) mint(adjust ...func(*apitoken.Token)) string {
	r.t.Helper()
	secret, hash := apitoken.NewSecret("own_")
	tok := apitoken.Token{
		ID: apitoken.NewID(), Name: "phone", Issuer: r.g.Provider().Issuer(),
		Subject: "u-7", DisplayName: "Sam", Groups: []string{"admins"}, CreatedAt: r.now,
	}
	for _, f := range adjust {
		f(&tok)
	}
	if err := r.tokens.CreateToken(r.t.Context(), tok, hash); err != nil {
		r.t.Fatal(err)
	}
	return secret
}

func (r *ownRig) call(method, path, cookie, bearer string) *http.Response {
	r.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		withCookie(req, "myapp_session", cookie)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return do(r.t, r.a.HTTP, req)
}

// The whole point: a token is recognised, and the gate's token routes are not
// there to duplicate the application's own.
func TestTheMiddleware_RecognisesWithoutMountingRoutes(t *testing.T) {
	r := newOwnRig(t, nil)
	secret := r.mint()

	if got := text(t, r.call(http.MethodGet, r.a.APIBase+"/open", "", secret)); got != "u-7 token" {
		t.Errorf("open route saw %q, want the maker, by token", got)
	}
	// Owner is derived from the groups, as behind a gate; the guard that wants a
	// browser still refuses by name.
	resp := r.call(http.MethodPost, r.a.APIBase+"/secrets", "", secret)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(text(t, resp), "token_not_allowed") {
		t.Errorf("a token on a browser-only route: status = %d, want 403 token_not_allowed", resp.StatusCode)
	}
	for _, path := range []string{r.g.MyTokensPath(), r.g.TokensPath()} {
		if resp := r.call(http.MethodGet, path, ownerCookie, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404: the gate must not mount its routes", path, resp.StatusCode)
		}
	}
}

// A token replaces a session, and the gate's own routes answer for the cookie
// alone — the two things the mount position is for.
func TestTheMiddleware_SitsWhereTheGatePutsIt(t *testing.T) {
	r := newOwnRig(t, nil)
	secret := r.mint()

	if got := text(t, r.call(http.MethodGet, r.a.APIBase+"/open", ownerCookie, secret)); got != "u-7 token" {
		t.Errorf("with a session and a token: %q, want the token to win", got)
	}
	who := decode[struct {
		Subject string `json:"subject"`
	}](t, r.call(http.MethodGet, r.g.WhoamiPath(), ownerCookie, secret))
	if who.Subject != "u-1" {
		t.Errorf("whoami answered for %q; it is mounted ahead of the middleware and goes by the cookie", who.Subject)
	}
}

// Every refusal the gate's own mount makes, this one makes too: they are the
// same code, and this is what holds them to it.
func TestTheMiddleware_RefusesWhatAGateRefuses(t *testing.T) {
	p := testProvider(t, "http://app.test")
	r := newOwnRig(t, &p)
	good := r.mint()
	expired := r.mint(func(tok *apitoken.Token) {
		at := r.now.Add(-time.Minute)
		tok.ExpiresAt = &at
	})
	elsewhere := r.mint(func(tok *apitoken.Token) { tok.Issuer = "https://other-idp.example.com" })

	for name, bearer := range map[string]string{
		"unknown": "own_never-made", "expired": expired, "made under another login": elsewhere,
	} {
		resp := r.call(http.MethodGet, r.a.APIBase+"/open", ownerCookie, bearer)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401, session or not", name, resp.StatusCode)
		}
	}

	// Somebody else's bearer value is not this application's business.
	before := r.tokens.lookups.Load()
	if got := text(t, r.call(http.MethodGet, r.a.APIBase+"/open", "", "myapp_not-ours")); got != "anon" {
		t.Errorf("another prefix: %q, want it passed over", got)
	}
	if n := r.tokens.lookups.Load(); n != before {
		t.Errorf("another prefix cost %d lookups, want none", n-before)
	}

	r.tokens.failLookup = errors.New("disk on fire")
	if resp := r.call(http.MethodGet, r.a.APIBase+"/open", "", good); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("store failure: status = %d, want 503", resp.StatusCode)
	}
}

func TestTheMiddleware_IsInertWithoutALogin(t *testing.T) {
	var none *oidcauth.Provider
	r := newOwnRig(t, &none)

	if got := text(t, r.call(http.MethodGet, r.a.APIBase+"/open", "", "own_anything")); got != "anon" {
		t.Errorf("no login: %q, want the request passed through", got)
	}
	if n := r.tokens.lookups.Load(); n != 0 {
		t.Errorf("no login cost %d lookups, want none", n)
	}

	// A nil provider func is "never configured", as it is for the gate.
	a := newTestApp(t)
	a.HTTP.Use(APITokenMiddleware(nil, r.tokens, "own_"))
	a.API.Get("/open", func(c fiber.Ctx) error { return c.SendString("ok") })
	req := httptest.NewRequest(http.MethodGet, a.APIBase+"/open", nil)
	req.Header.Set("Authorization", "Bearer own_anything")
	if resp := do(t, a.HTTP, req); resp.StatusCode != http.StatusOK {
		t.Errorf("nil provider: status = %d, want 200", resp.StatusCode)
	}
}

// An empty prefix would claim every bearer value and answer 401 to credentials
// meant for something else, so it is refused where it is wired.
func TestTheMiddleware_RefusesToBeMiswired(t *testing.T) {
	for name, build := range map[string]func(){
		"no store":  func() { APITokenMiddleware(nil, nil, "own_") },
		"no prefix": func() { APITokenMiddleware(nil, &spyTokens{}, "") },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("built a middleware that cannot work")
				}
			}()
			build()
		})
	}
}

// ── Cost ────────────────────────────────────────────────────────────────────

// The path without a token is nearly every request. It must match
// BenchmarkBaselineNoMiddleware's allocation count: one header read, no store.
func BenchmarkResolveAPITokenNoHeader(b *testing.B) {
	p, _ := oidcauth.New(oidcauth.Config{
		Issuer: "https://id.example.com", ClientID: "app", ClientSecret: "s",
		BaseURL: "https://app.example.com",
	})
	g := New(newBenchApp(b), Options{
		Provider: func() *oidcauth.Provider { return p },
		Sessions: newMemStore(), CookiePrefix: "myapp", RateLimit: -1,
		Tokens: &spyTokens{},
	})
	app := benchApp(g, g.ResolveAPIToken())
	runBench(b, app, func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) })
}
