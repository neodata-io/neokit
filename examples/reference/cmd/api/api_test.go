package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/session"
)

// These tests drive the service exactly as main builds it — the real wiring,
// a real database file, the real middleware chain — through Fiber's in-memory
// transport, so nothing listens on a port. What they pin is the wiring itself:
// each layer has tests of its own for its rules.

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{Base: config.Base{
		DatabasePath: filepath.Join(t.TempDir(), "test.db"),
		LogLevel:     "error", LogFormat: "json",
	}}
}

func start(t *testing.T, cfg Config) *service {
	t.Helper()
	svc, err := wire(cfg)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	t.Cleanup(func() { _ = svc.app.Close() })
	return svc
}

// caller is who a request comes from: a browser session, an API token, or
// nobody.
type caller struct{ cookie, bearer string }

func (s *service) do(t *testing.T, who caller, method, path string, body any, headers ...string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if who.cookie != "" {
		req.AddCookie(&http.Cookie{Name: "reference_session", Value: who.cookie})
	}
	if who.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+who.bearer)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := s.app.HTTP.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func read[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return v
}

type task struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Done  bool   `json:"done"`
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// ── The open service ────────────────────────────────────────────────────────

func TestATaskFromCreationToDeletion(t *testing.T) {
	s := start(t, testConfig(t))
	base := s.app.APIBase + "/tasks"
	nobody := caller{}

	created := s.do(t, nobody, http.MethodPost, base, map[string]any{"title": "write the report"})
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d", created.StatusCode)
	}
	made := read[task](t, created)
	// 201 names what it made, and hands out the version to edit against.
	if got, want := created.Header.Get("Location"), base+"/"+made.ID; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	etag := created.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the created task")
	}

	// Someone else completes it…
	if resp := s.do(t, nobody, http.MethodPatch, base+"/"+made.ID, map[string]any{"done": true}); resp.StatusCode != http.StatusOK {
		t.Fatalf("complete: status = %d", resp.StatusCode)
	}
	// …so an edit made against the version from before is refused, by name.
	stale := s.do(t, nobody, http.MethodPatch, base+"/"+made.ID, map[string]any{"title": "overwrite"}, "If-Match", etag)
	if e := read[apiError](t, stale); stale.StatusCode != http.StatusPreconditionFailed || e.Code != "stale" {
		t.Errorf("stale edit: status = %d, code = %q; want 412 stale", stale.StatusCode, e.Code)
	}

	list := read[[]task](t, s.do(t, nobody, http.MethodGet, base, nil))
	if len(list) != 1 || !list[0].Done || list[0].Title != "write the report" {
		t.Errorf("list = %+v, want the one task, done, title untouched", list)
	}

	if resp := s.do(t, nobody, http.MethodDelete, base+"/"+made.ID, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete: status = %d, want 204", resp.StatusCode)
	}
	gone := s.do(t, nobody, http.MethodGet, base+"/"+made.ID, nil)
	if e := read[apiError](t, gone); gone.StatusCode != http.StatusNotFound || e.Code != "not_found" {
		t.Errorf("after delete: status = %d, code = %q; want 404 not_found", gone.StatusCode, e.Code)
	}
}

// Every failure leaves as the same envelope with a stable code, whichever layer
// refused: the binder, the validator, or the domain.
func TestFailuresShareOneEnvelope(t *testing.T) {
	s := start(t, testConfig(t))
	base := s.app.APIBase + "/tasks"

	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"a field the validator rejects", map[string]any{"title": ""}, 400, "validation_failed"},
		{"a title the domain rejects", map[string]any{"title": "   "}, 400, "invalid_input"},
		{"a body that is not JSON", "{", 400, "invalid_body"},
	}
	for _, tc := range cases {
		var resp *http.Response
		if raw, ok := tc.body.(string); ok {
			req := httptest.NewRequest(http.MethodPost, base, strings.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			resp, _ = s.app.HTTP.Test(req)
		} else {
			resp = s.do(t, caller{}, http.MethodPost, base, tc.body)
		}
		e := read[apiError](t, resp)
		if resp.StatusCode != tc.status || e.Code != tc.code || e.Error == "" {
			t.Errorf("%s: status = %d, body = %+v; want %d %s", tc.name, resp.StatusCode, e, tc.status, tc.code)
		}
	}
}

// The service is open in this test, which is exactly when this matters: with no
// login, nothing else stands between a page on another site and a write made
// through a visitor's browser.
func TestAnotherSiteCannotWriteEvenWhileTheServiceIsOpen(t *testing.T) {
	s := start(t, testConfig(t))

	resp := s.do(t, caller{}, http.MethodPost, s.app.APIBase+"/tasks", map[string]any{"title": "planted"},
		"Sec-Fetch-Site", "cross-site", "Origin", "https://evil.example")
	if e := read[apiError](t, resp); resp.StatusCode != http.StatusForbidden || e.Code != "cross_origin" {
		t.Errorf("status = %d, code = %q; want 403 cross_origin", resp.StatusCode, e.Code)
	}
	if list := read[[]task](t, s.do(t, caller{}, http.MethodGet, s.app.APIBase+"/tasks", nil)); len(list) != 0 {
		t.Errorf("the planted task was stored: %+v", list)
	}
}

func TestProbes(t *testing.T) {
	s := start(t, testConfig(t))
	for _, path := range []string{"/healthz", ReadyPath} {
		if resp := s.do(t, caller{}, http.MethodGet, path, nil); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, resp.StatusCode)
		}
	}
	// Not ready is a 503 that names nothing: the detail goes to the log.
	s.store.Close()
	resp := s.do(t, caller{}, http.MethodGet, ReadyPath, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(body), "database") {
		t.Errorf("with the database closed: status = %d, body = %s; want a bare 503", resp.StatusCode, body)
	}
}

// The database has to close after everything that uses it. wire pushes it
// first, so the stack — which unwinds in reverse — reaches it last of the
// service's own steps.
func TestTheDatabaseIsOnTheShutdownStack(t *testing.T) {
	s := start(t, testConfig(t))
	names := s.app.Shutdown.Names()
	at := slices.Index(names, "database")
	if at < 0 {
		t.Fatalf("shutdown steps = %v: the database is never closed", names)
	}
	// Tracing and metrics are pushed by the app before anything of ours, so they
	// flush after the database closes and still export a span from that close.
	for _, earlier := range []string{"tracing", "metrics-export"} {
		if i := slices.Index(names, earlier); i < 0 || i > at {
			t.Errorf("shutdown steps = %v: %q must be pushed before the database", names, earlier)
		}
	}
}

// ── The same service with a login ───────────────────────────────────────────

// withLogin starts the service with a login configured and two people signed
// in. No identity provider is contacted: a session is a row, so the test writes
// the two rows a real sign-in would have.
func withLogin(t *testing.T) (s *service, member, owner caller) {
	t.Helper()
	cfg := testConfig(t)
	cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret = "https://id.example.com", "reference", "secret"
	cfg.OIDCBaseURL, cfg.OIDCOwnerGroup = "http://localhost:8080", "admin"
	s = start(t, cfg)

	sessions, err := session.NewSQLite(s.store.DB())
	if err != nil {
		t.Fatal(err)
	}
	signIn := func(subject string, groups ...string) caller {
		now := time.Now().UTC()
		token := session.RandomToken(32)
		err := sessions.CreateSession(t.Context(), session.Session{
			ID: "sess-" + subject, Subject: subject, Name: subject, Groups: groups,
			CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
		}, session.HashToken(token))
		if err != nil {
			t.Fatal(err)
		}
		return caller{cookie: token}
	}
	return s, signIn("mira", "staff"), signIn("ruben", "staff", "admin")
}

func TestWithALoginTheSameRoutesNeedOne(t *testing.T) {
	s, member, owner := withLogin(t)
	base := s.app.APIBase + "/tasks"

	if resp := s.do(t, caller{}, http.MethodGet, base, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: status = %d, want 401", resp.StatusCode)
	}

	made := read[task](t, s.do(t, member, http.MethodPost, base, map[string]any{"title": "write the report"}))
	if made.ID == "" {
		t.Fatal("a signed-in member could not create a task")
	}
	// Deleting is the owner's: the module said so where it declared the route.
	if resp := s.do(t, member, http.MethodDelete, base+"/"+made.ID, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("member deleting: status = %d, want 403", resp.StatusCode)
	}
	if resp := s.do(t, owner, http.MethodDelete, base+"/"+made.ID, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("owner deleting: status = %d, want 204", resp.StatusCode)
	}
}

// A script acts as the person who made its token — and cannot use it to make
// another.
func TestAScriptUsesAPersonalToken(t *testing.T) {
	s, member, _ := withLogin(t)

	minted := s.do(t, member, http.MethodPost, s.gate.MyTokensPath(), map[string]any{"name": "nightly import"})
	if minted.StatusCode != http.StatusCreated {
		t.Fatalf("mint: status = %d", minted.StatusCode)
	}
	token := read[struct {
		Token     string  `json:"token"`
		ExpiresAt *string `json:"expiresAt"`
	}](t, minted)
	if !strings.HasPrefix(token.Token, "reference_") {
		t.Errorf("token %q does not carry the service's prefix", token.Token)
	}
	// wire sets a maximum lifetime, so a token asked for without one still ends.
	if token.ExpiresAt == nil {
		t.Error("the token has no expiry although the service sets a maximum")
	}

	script := caller{bearer: token.Token}
	if resp := s.do(t, script, http.MethodPost, s.app.APIBase+"/tasks", map[string]any{"title": "imported"}); resp.StatusCode != http.StatusCreated {
		t.Errorf("the script creating a task: status = %d, want 201", resp.StatusCode)
	}
	again := s.do(t, script, http.MethodPost, s.gate.MyTokensPath(), map[string]any{"name": "another"})
	if e := read[apiError](t, again); again.StatusCode != http.StatusForbidden || e.Code != "token_not_allowed" {
		t.Errorf("a token minting a token: status = %d, code = %q; want 403 token_not_allowed", again.StatusCode, e.Code)
	}
}
