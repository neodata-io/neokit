# Session Store Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a SQLite-backed session store in neokit so `neokit.App.Login` works without the caller writing six interface methods and a schema.

**Architecture:** The session vocabulary (`Session`, `Identity`, `Store`, `ExpiredSweeper`, `Policy`, `HashToken`, `RandomToken`) moves out of `oidcauth` into a new standard-library-only package `session`, so an application with a password login can use it without linking `go-oidc` and `oauth2`. `oidcauth` keeps every moved name as a type alias or a one-line wrapper, so existing consumers compile unchanged. `session` then ships one implementation of `Store` over `*sql.DB` speaking SQLite.

**Tech Stack:** Go 1.25, `database/sql`, `modernc.org/sqlite` (test-only in this package), `crypto/sha256`, `crypto/rand`.

**Spec:** `docs/superpowers/specs/2026-08-03-session-store-design.md`

## Global Constraints

- **Module path is `github.com/neodata-io/neokit`.** Imports are absolute from there.
- **The `session` package imports only the standard library.** No neokit package, no third-party module. `modernc.org/sqlite` appears in `session`'s *test* files only, as a blank driver import.
- **The raw session token is never written to the database.** Only `HashToken`'s output.
- **The store takes no clock.** Every time value it persists arrives as a parameter.
- **Table name is `auth_session`**, columns and RFC3339-UTC encoding exactly as in the spec's Schema section, because NeoGate's existing rows must fit it without a data migration.
- **DDL runs as `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`** at construction. Never through `sqlitex.Migrate` — that counter belongs to the application.
- **Comment density: 1–3 lines.** Explain *why*, not *what*. Do not write paragraph-length comments.
- **Test databases are files under `t.TempDir()`**, never bare `:memory:` — a memory database is private per pooled connection, so a write and a read can land on different databases.
- **Every task ends green:** `go build ./... && go vet ./... && go test ./...` from the repository root.

---

### Task 1: The `session` package vocabulary

Move the types out of `oidcauth` and leave aliases behind. Nothing changes for any existing caller; this task exists so Task 2 has a home that does not drag OIDC into a password-login application.

**Files:**
- Create: `session/session.go`
- Create: `session/policy.go`
- Create: `session/token.go`
- Create: `session/policy_test.go`
- Create: `oidcauth/alias_test.go`
- Modify: `oidcauth/identity.go` (delete the moved declarations, add aliases)

**Interfaces:**
- Consumes: nothing.
- Produces: `session.Session`, `session.Identity`, `session.Store`, `session.ExpiredSweeper`, `session.Policy`, `session.DefaultPolicy() Policy`, `session.HashToken(string) string`, `session.RandomToken(int) (string, error)`. Task 2 onward implements `session.Store` and `session.ExpiredSweeper`.

- [ ] **Step 1: Create `session/session.go`**

Copy `Identity`, `Identity.Authenticated`, `Session`, `Session.Identity`, `SessionStore` (renamed `Store`) and `ExpiredSweeper` verbatim from `oidcauth/identity.go:13-89`, keeping their doc comments. Three edits to those comments:

- `Identity.Owner` currently says "membership in `[Config.OwnerGroup]`". `Config` is an `oidcauth` type that does not exist here — reword to "membership in the configured owner group, or true for every authenticated identity when none is configured."
- `Store`'s doc currently says "this package ships none". It now does — reword as below.
- `Session`'s reference to `[HashToken]` stays; the function moves into this package in Step 3.

```go
// Package session is the vocabulary for a signed-in browser, and one store that
// persists it.
//
// It imports only the standard library so that an application authenticating by
// password can use it without linking an OpenID Connect relying party. [oidcauth]
// aliases every name here, so the two are the same types rather than two sets
// that must be kept in step.
package session

import (
	"context"
	"time"
)

// Identity is the authenticated principal. A zero value (Subject == "") means
// "not authenticated".
type Identity struct {
	// Subject is the stable user id from the provider (the `sub` claim). It is
	// the only claim guaranteed to be present and immutable — key your own user
	// records on this, never on Email, which a provider may let a user change.
	Subject string
	// Name is a display name; it falls back to Subject when the provider sends
	// none, so it is never empty for an authenticated identity.
	Name string
	// Email is the address, when the provider supplies one.
	Email string
	// Groups is the group membership from the configured claim.
	Groups []string
	// Owner reports administrative rights: membership in the configured owner
	// group, or true for every authenticated identity when none is configured.
	Owner bool
}

// Authenticated reports whether the identity names anyone.
func (i Identity) Authenticated() bool { return i.Subject != "" }

// Session is a signed-in browser: the record consulted on every request, so the
// provider is not involved again until it expires.
//
// The cookie's token is deliberately absent. Only its SHA-256 is stored (see
// [HashToken]), so a database leak yields nothing that can be replayed as a
// session.
type Session struct {
	ID         string
	Subject    string
	Name       string
	Email      string
	Groups     []string
	Owner      bool
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// Identity projects the session back to the principal it was minted for.
func (s Session) Identity() Identity {
	return Identity{
		Subject: s.Subject, Name: s.Name, Email: s.Email,
		Groups: s.Groups, Owner: s.Owner,
	}
}

// Store persists sessions. [SQLite] is the implementation this package ships;
// implement it yourself over whatever storage you already have.
//
// tokenHash is always the output of [HashToken]; the raw token must never be
// written down.
//
// An implementation may filter expired rows on read, but is not required to:
// expiry is re-checked by the middleware, so the security property does not
// depend on which store is wired in.
type Store interface {
	CreateSession(ctx context.Context, s Session, tokenHash string) error
	SessionByToken(ctx context.Context, tokenHash string) (Session, bool, error)
	TouchSession(ctx context.Context, id string, lastSeen, expires time.Time) error
	DeleteSession(ctx context.Context, id string) error
	DeleteSessionByToken(ctx context.Context, tokenHash string) error
	ListSessions(ctx context.Context) ([]Session, error)
}

// ExpiredSweeper is the optional companion to [Store]: bulk collection of dead
// rows, for a store that can do it in one statement.
//
// It is separate because it is housekeeping rather than a security control —
// expiry is enforced on read, so a row that outlives its expiry authenticates
// nobody even if the sweep never runs.
type ExpiredSweeper interface {
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}
```

- [ ] **Step 2: Create `session/policy.go`**

Move `Policy`, `DefaultPolicy`, `withDefaults`, `ExpiryFor`, `Live` and `NeedsTouch` from `oidcauth/identity.go:91-183` verbatim, including their doc comments. One comment edit: `Live`'s doc says "`[SessionStore]` is an interface" — change the link to `[Store]`.

```go
package session

import "time"

// Policy is the session lifetime rules. Use [DefaultPolicy] and adjust.
type Policy struct {
	// TTL is how long a session lives without use. Every visit rolls it forward.
	TTL time.Duration

	// MaxLifetime caps a session's total age no matter how active it is.
	//
	// Without it the sliding renewal has no ceiling, so a browser checking in
	// once a month stays signed in forever. Owner and Groups are the snapshot
	// taken at login and are never re-derived, so this is what eventually forces
	// a fresh handshake — the real bound on how long a revoked identity keeps
	// access, and the cost of raising it.
	MaxLifetime time.Duration

	// TouchInterval is the minimum gap between last-seen writes. Without it every
	// authenticated request would write to the database.
	TouchInterval time.Duration
}
```

Then the five functions, copied unchanged from `oidcauth/identity.go:110-183`.

- [ ] **Step 3: Create `session/token.go`**

Move `HashToken` and `RandomToken` from `oidcauth/identity.go:185-191` and `238-246`. The error prefix changes from `oidc:` to `session:` — no test asserts that string (verified: the only other match in the repo is `ids/tokengen.go`, unrelated).

```go
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// HashToken is the one-way transform between the cookie's token and what the
// store holds. It is exported so a login handler, a resolver, and any
// out-of-band session tool can never disagree about it.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RandomToken returns n bytes of URL-safe randomness, for a session token, a
// state, a nonce or a PKCE verifier.
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: crypto/rand failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
```

- [ ] **Step 4: Rewrite `oidcauth/identity.go` to alias**

Delete lines 13-191 and 238-246 (everything moved). Keep `Handshake`, `NewHandshake` and `Handshake.Valid` — they are OIDC-specific and stay. Add the alias block at the top. `Handshake` calls `RandomToken`, which still resolves through the wrapper.

```go
package oidcauth

import (
	"fmt"

	"github.com/neodata-io/neokit/session"
)

// The session vocabulary lives in [session], which imports only the standard
// library — so an application authenticating by password can use it without
// linking this package's OIDC client. These are aliases, not copies: a store
// written against either name satisfies both.
type (
	Identity       = session.Identity
	Session        = session.Session
	SessionStore   = session.Store
	ExpiredSweeper = session.ExpiredSweeper
	Policy         = session.Policy
)

// DefaultPolicy is a 30-day idle expiry under a 30-day absolute cap, with
// activity recorded at most hourly. See [session.DefaultPolicy].
func DefaultPolicy() Policy { return session.DefaultPolicy() }

// HashToken is the one-way transform between the cookie's token and what the
// store holds. See [session.HashToken].
func HashToken(token string) string { return session.HashToken(token) }

// RandomToken returns n bytes of URL-safe randomness. See [session.RandomToken].
func RandomToken(n int) (string, error) { return session.RandomToken(n) }
```

Keep the existing `Handshake` declarations below this block, unchanged. Remove now-unused imports from the file (`context`, `crypto/rand`, `crypto/sha256`, `encoding/hex`, `time`); `fmt` and `encoding/base64` are still needed only if `Handshake` uses them — it does not directly, so drop everything the remaining code no longer references and let `go build` confirm.

- [ ] **Step 5: Verify the move compiles and existing tests still pass**

Run: `go build ./... && go test ./oidcauth/... ./...`
Expected: PASS. `oidcauth/config_test.go` exercises `Policy.Live`, `ExpiryFor`, the zero-field fallback and `HashToken` through the alias — those four tests passing unchanged *is* the proof that the alias is transparent.

- [ ] **Step 6: Write the alias compile-time test**

Create `oidcauth/alias_test.go`. A runtime assertion cannot prove type identity; assignment in both directions can, at compile time.

```go
package oidcauth_test

import (
	"testing"

	"github.com/neodata-io/neokit/oidcauth"
	"github.com/neodata-io/neokit/session"
)

// The aliases are the compatibility guarantee, and only assignment in both
// directions proves they are one type rather than two identical ones. A store
// written against either name has to satisfy both.
func TestOIDCNamesAreTheSessionTypes(t *testing.T) {
	var (
		s  session.Session  = oidcauth.Session{Subject: "u1"}
		_  oidcauth.Session = s
		i  session.Identity = oidcauth.Identity{Subject: "u1"}
		_  oidcauth.Identity = i
		p  session.Policy   = oidcauth.DefaultPolicy()
		_  oidcauth.Policy  = p
	)
	var st session.Store
	var _ oidcauth.SessionStore = st

	if oidcauth.HashToken("t") != session.HashToken("t") {
		t.Error("oidcauth.HashToken must be session.HashToken")
	}
}
```

- [ ] **Step 7: Write the policy tests in `session`**

Create `session/policy_test.go`. `oidcauth/config_test.go` already covers this through the alias; these exist so the package is testable on its own terms and so a future removal of the alias does not remove the coverage with it.

```go
package session_test

import (
	"testing"
	"time"

	"github.com/neodata-io/neokit/session"
)

// The sliding TTL must never push expiry past the absolute cap: a row that
// outlives the cap is rejected at read time but never *expires*, so no sweep
// ever collects it.
func TestExpiryForClampsToTheAbsoluteCap(t *testing.T) {
	p := session.Policy{TTL: 30 * 24 * time.Hour, MaxLifetime: 24 * time.Hour}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := created.Add(12 * time.Hour)

	got := p.ExpiryFor(created, now)
	if want := created.Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("ExpiryFor = %v, want the cap %v", got, want)
	}
}

// Live is the enforcement point. It re-checks both bounds rather than trusting
// the store, because Store is an interface and a security property must not
// depend on which implementation is wired in.
func TestLiveRejectsExpiredAndOverAgedSessions(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	p := session.Policy{TTL: time.Hour, MaxLifetime: 24 * time.Hour, TouchInterval: time.Hour}

	expired := session.Session{CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Minute)}
	if p.Live(expired, now) {
		t.Error("a session past ExpiresAt must not be live")
	}

	overAged := session.Session{CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(time.Hour)}
	if p.Live(overAged, now) {
		t.Error("a session past MaxLifetime must not be live, however recently it was used")
	}

	fresh := session.Session{CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	if !p.Live(fresh, now) {
		t.Error("a session inside both bounds must be live")
	}
}

// A zero field means "unset", so one override does not force restating the rest.
func TestZeroFieldsFallBackToDefaults(t *testing.T) {
	got := session.Policy{TTL: time.Minute}.ExpiryFor(time.Time{}, time.Unix(0, 0).UTC())
	if want := time.Unix(0, 0).UTC().Add(time.Minute); !got.Equal(want) {
		t.Errorf("ExpiryFor = %v, want the supplied TTL honoured at %v", got, want)
	}
	if session.DefaultPolicy().TouchInterval != time.Hour {
		t.Error("DefaultPolicy must record activity at most hourly")
	}
}

// The hash is what the store holds, so it must be stable and must not be the
// token.
func TestHashTokenIsStableAndNotTheToken(t *testing.T) {
	if session.HashToken("abc") != session.HashToken("abc") {
		t.Error("HashToken must be stable")
	}
	if session.HashToken("abc") == "abc" {
		t.Error("HashToken must not return the token")
	}
	if session.HashToken("abc") == session.HashToken("abd") {
		t.Error("HashToken must distinguish different tokens")
	}
}
```

- [ ] **Step 8: Run the tests**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, all packages.

- [ ] **Step 9: Commit**

```bash
git add session/ oidcauth/identity.go oidcauth/alias_test.go
git commit -m "refactor(oidcauth)!: the session vocabulary moves to a stdlib-only package

oidcauth links go-oidc and oauth2, so an application authenticating by
password could not implement SessionStore without linking an OIDC client.
The types move to session; oidcauth aliases every one of them, so no
caller changes."
```

---

### Task 2: `session.SQLite` — construction and schema

**Files:**
- Create: `session/sqlite.go`
- Create: `session/sqlite_test.go`

**Interfaces:**
- Consumes: `session.Store`, `session.Session` from Task 1.
- Produces: `session.SQLite` struct and `session.NewSQLite(db *sql.DB) (*SQLite, error)`. Tasks 3-5 add methods to `*SQLite`.

- [ ] **Step 1: Write the failing test**

Create `session/sqlite_test.go`:

```go
package session_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // the driver the tests open with

	"github.com/neodata-io/neokit/session"
)

// A file under TempDir, never a bare :memory: — a memory database is private
// per pooled connection, so a write and a read can land on different databases.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The DDL runs on every boot, so it has to be idempotent — that is what lets
// construction create the schema instead of an application migration doing it.
func TestNewSQLiteIsIdempotent(t *testing.T) {
	db := testDB(t)

	if _, err := session.NewSQLite(db); err != nil {
		t.Fatalf("first NewSQLite: %v", err)
	}
	if _, err := session.NewSQLite(db); err != nil {
		t.Fatalf("second NewSQLite: %v", err)
	}

	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'auth_session'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("auth_session was not created: %v", err)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./session/ -run TestNewSQLiteIsIdempotent -v`
Expected: FAIL — `undefined: session.NewSQLite`.

- [ ] **Step 3: Write the implementation**

Create `session/sqlite.go`:

```go
package session

import (
	"database/sql"
	"fmt"
)

// timeLayout is how instants are stored: RFC3339 in UTC, which sorts
// lexicographically, so the sweep's ranged DELETE works on a TEXT column.
const timeLayout = "2006-01-02T15:04:05Z07:00"

// schema is applied on every construction. IF NOT EXISTS rather than a
// sqlitex.Migrate step because that step's index is its version and the
// application owns that sequence — inserting one here would shift every later
// version in every database already in the field.
const schema = `
CREATE TABLE IF NOT EXISTS auth_session (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL UNIQUE,
    subject      TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    email        TEXT NOT NULL DEFAULT '',
    groups_json  TEXT NOT NULL DEFAULT '[]',
    owner        INTEGER NOT NULL DEFAULT 0,
    user_agent   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    expires_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS auth_session_expires ON auth_session(expires_at);
`

// SQLite is a [Store] over any *sql.DB speaking SQLite. It also implements
// [ExpiredSweeper], so a gate schedules the sweep without being asked.
//
// It holds no clock: every instant it writes arrives as a parameter, which
// leaves expiry enforced in the one place already tested for it, [Policy.Live].
type SQLite struct{ db *sql.DB }

// NewSQLite creates the session table and its indexes, then returns the store.
//
// The column names and the RFC3339 encoding are chosen so an existing
// auth_session table fits without a data migration.
func NewSQLite(db *sql.DB) (*SQLite, error) {
	if db == nil {
		return nil, fmt.Errorf("session: NewSQLite needs a database")
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("session: create schema: %w", err)
	}
	return &SQLite{db: db}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./session/ -run TestNewSQLiteIsIdempotent -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add session/sqlite.go session/sqlite_test.go
git commit -m "feat(session): a SQLite store that creates its own schema"
```

---

### Task 3: Create and read a session

The two methods on the hot path, and the one test whose failure is a security bug.

**Files:**
- Modify: `session/sqlite.go`
- Modify: `session/sqlite_test.go`

**Interfaces:**
- Consumes: `session.NewSQLite` from Task 2.
- Produces: `(*SQLite).CreateSession(ctx, Session, tokenHash string) error` and `(*SQLite).SessionByToken(ctx, tokenHash string) (Session, bool, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `session/sqlite_test.go`:

```go
// A stamped session, so every column has something to round-trip.
func aSession() session.Session {
	t0 := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	return session.Session{
		ID: "sess-1", Subject: "user-1", Name: "Ada", Email: "ada@example.com",
		Groups: []string{"admins", "staff"}, Owner: true, UserAgent: "Firefox",
		CreatedAt: t0, LastSeenAt: t0, ExpiresAt: t0.Add(24 * time.Hour),
	}
}

func TestCreateAndResolveByToken(t *testing.T) {
	store, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	want := aSession()
	if err := store.CreateSession(context.Background(), want, session.HashToken("raw-token")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, ok, err := store.SessionByToken(context.Background(), session.HashToken("raw-token"))
	if err != nil || !ok {
		t.Fatalf("SessionByToken = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if got.ID != want.ID || got.Subject != want.Subject || got.Email != want.Email ||
		got.Owner != want.Owner || got.UserAgent != want.UserAgent {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Groups) != 2 || got.Groups[0] != "admins" {
		t.Errorf("Groups = %v, want [admins staff]", got.Groups)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
}

// The property the whole design exists for. If this fails, a stolen database
// file is a stolen login — so it reads every column of every row rather than
// trusting that the insert named the right ones.
func TestTheRawTokenNeverReachesTheDatabase(t *testing.T) {
	db := testDB(t)
	store, err := session.NewSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	const raw = "a-very-distinctive-raw-token"
	if err := store.CreateSession(context.Background(), aSession(), session.HashToken(raw)); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT * FROM auth_session`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			t.Fatal(err)
		}
		for i, c := range cells {
			if v := c.(*sql.NullString); v.Valid && strings.Contains(v.String, raw) {
				t.Fatalf("column %q holds the raw token", cols[i])
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// Absent is not an error: a cookie for a session that was revoked or swept is
// the ordinary case, and reporting it as a failure would make every logged-out
// visitor an error line.
func TestAnUnknownTokenIsNotAnError(t *testing.T) {
	store, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.SessionByToken(context.Background(), session.HashToken("never-issued"))
	if err != nil {
		t.Fatalf("SessionByToken error = %v, want nil", err)
	}
	if ok {
		t.Errorf("SessionByToken ok = true for an unknown token, got %+v", got)
	}
}

// An expired row is returned rather than filtered. Expiry is Policy.Live's job;
// a second enforcement point here is how the two come to disagree.
func TestAnExpiredRowIsReturnedAndRejectedByPolicy(t *testing.T) {
	store, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	dead := aSession()
	dead.ExpiresAt = dead.CreatedAt.Add(-time.Hour)
	if err := store.CreateSession(context.Background(), dead, session.HashToken("t")); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.SessionByToken(context.Background(), session.HashToken("t"))
	if err != nil || !ok {
		t.Fatalf("the store must return the row, got (_, %v, %v)", ok, err)
	}
	if session.DefaultPolicy().Live(got, dead.CreatedAt) {
		t.Error("Policy.Live must reject the expired row the store returned")
	}
}
```

Add `"context"`, `"strings"` and `"time"` to the test file's imports.

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./session/ -run 'TestCreateAndResolve|TestTheRawToken|TestAnUnknown|TestAnExpiredRow' -v`
Expected: FAIL — `store.CreateSession undefined`.

- [ ] **Step 3: Write the implementation**

Append to `session/sqlite.go`, and add `"context"`, `"encoding/json"`, `"errors"`, `"time"` to its imports:

```go
// CreateSession stores a new signed-in browser. tokenHash is [HashToken]'s
// output; the token itself never reaches the database.
func (s *SQLite) CreateSession(ctx context.Context, sess Session, tokenHash string) error {
	groups, err := json.Marshal(sess.Groups)
	if err != nil {
		return fmt.Errorf("session: encode groups: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO auth_session
			(id, token_hash, subject, name, email, groups_json, owner, user_agent,
			 created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, tokenHash, sess.Subject, sess.Name, sess.Email, string(groups),
		sess.Owner, sess.UserAgent,
		stamp(sess.CreatedAt), stamp(sess.LastSeenAt), stamp(sess.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("session: create: %w", err)
	}
	return nil
}

// SessionByToken resolves a session from the cookie's hash. A row past its
// expiry is returned like any other — [Policy.Live] is the enforcement point,
// and a second one here is how the two come to disagree.
func (s *SQLite) SessionByToken(ctx context.Context, tokenHash string) (Session, bool, error) {
	row := s.db.QueryRowContext(ctx, selectColumns+` WHERE token_hash = ?`, tokenHash)
	sess, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("session: read: %w", err)
	}
	return sess, true, nil
}

// selectColumns fixes the column order every scan depends on, so the list and
// the single-row read cannot drift apart.
const selectColumns = `
	SELECT id, subject, name, email, groups_json, owner, user_agent,
	       created_at, last_seen_at, expires_at
	FROM auth_session`

// scanner is what QueryRow and Rows have in common, so one scan serves both.
type scanner interface{ Scan(dest ...any) error }

func scan(r scanner) (Session, error) {
	var (
		s          Session
		groups     string
		created    string
		lastSeen   string
		expires    string
	)
	if err := r.Scan(&s.ID, &s.Subject, &s.Name, &s.Email, &groups, &s.Owner,
		&s.UserAgent, &created, &lastSeen, &expires); err != nil {
		return Session{}, err
	}
	// A row whose groups will not parse yields no groups rather than no session:
	// losing a group is a lesser failure than locking someone out.
	_ = json.Unmarshal([]byte(groups), &s.Groups)
	s.CreatedAt = parse(created)
	s.LastSeenAt = parse(lastSeen)
	s.ExpiresAt = parse(expires)
	return s, nil
}

// stamp and parse are the only place the storage encoding is decided.
func stamp(t time.Time) string { return t.UTC().Format(timeLayout) }

func parse(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./session/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add session/sqlite.go session/sqlite_test.go
git commit -m "feat(session): create and resolve a session, storing only the hash"
```

---

### Task 4: Touch, revoke and list

**Files:**
- Modify: `session/sqlite.go`
- Modify: `session/sqlite_test.go`

**Interfaces:**
- Consumes: everything from Task 3, including `scan`, `stamp`, `parse` and `selectColumns`.
- Produces: `(*SQLite).TouchSession`, `(*SQLite).DeleteSession`, `(*SQLite).DeleteSessionByToken`, `(*SQLite).ListSessions`. After this task `*SQLite` satisfies `session.Store`.

- [ ] **Step 1: Write the failing tests**

Append to `session/sqlite_test.go`:

```go
// Touch rolls activity and expiry forward and must leave everything else alone —
// it runs on ordinary requests, where rewriting identity would be a silent
// privilege change.
func TestTouchMovesOnlyTheTimestamps(t *testing.T) {
	store, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	orig := aSession()
	if err := store.CreateSession(context.Background(), orig, session.HashToken("t")); err != nil {
		t.Fatal(err)
	}

	seen := orig.LastSeenAt.Add(2 * time.Hour)
	exp := orig.ExpiresAt.Add(2 * time.Hour)
	if err := store.TouchSession(context.Background(), orig.ID, seen, exp); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}

	got, _, err := store.SessionByToken(context.Background(), session.HashToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSeenAt.Equal(seen) || !got.ExpiresAt.Equal(exp) {
		t.Errorf("timestamps = (%v, %v), want (%v, %v)", got.LastSeenAt, got.ExpiresAt, seen, exp)
	}
	if got.Subject != orig.Subject || got.Owner != orig.Owner || !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("Touch changed identity or creation: %+v", got)
	}
}

// Revocation by id is the "sign out this device" path; by token is logout.
func TestDeleteByIDAndByToken(t *testing.T) {
	store, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	first, second := aSession(), aSession()
	second.ID = "sess-2"
	if err := store.CreateSession(context.Background(), first, session.HashToken("t1")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), second, session.HashToken("t2")); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteSession(context.Background(), first.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, ok, _ := store.SessionByToken(context.Background(), session.HashToken("t1")); ok {
		t.Error("DeleteSession left the row")
	}
	if _, ok, _ := store.SessionByToken(context.Background(), session.HashToken("t2")); !ok {
		t.Error("DeleteSession removed the wrong row")
	}

	if err := store.DeleteSessionByToken(context.Background(), session.HashToken("t2")); err != nil {
		t.Fatalf("DeleteSessionByToken: %v", err)
	}
	if _, ok, _ := store.SessionByToken(context.Background(), session.HashToken("t2")); ok {
		t.Error("DeleteSessionByToken left the row")
	}
}

// Deleting a session that is already gone is success: logging out twice, or
// revoking a device that expired first, is not a failure to report.
func TestDeletingAnAbsentSessionSucceeds(t *testing.T) {
	store, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSession(context.Background(), "no-such-id"); err != nil {
		t.Errorf("DeleteSession on an absent row = %v, want nil", err)
	}
	if err := store.DeleteSessionByToken(context.Background(), session.HashToken("nope")); err != nil {
		t.Errorf("DeleteSessionByToken on an absent row = %v, want nil", err)
	}
}

// List includes expired rows. Its caller is a "your devices" screen, which has
// to show a session in order to offer revoking it.
func TestListReturnsEveryRowIncludingExpired(t *testing.T) {
	store, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	live := aSession()
	dead := aSession()
	dead.ID, dead.ExpiresAt = "sess-dead", live.CreatedAt.Add(-time.Hour)
	if err := store.CreateSession(context.Background(), live, session.HashToken("t1")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), dead, session.HashToken("t2")); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSessions returned %d rows, want 2 including the expired one", len(got))
	}
}

// The interface is the contract fiberauth is written against, so satisfying it
// has to be a compile error when it stops being true.
func TestSQLiteIsAStore(t *testing.T) {
	var _ session.Store = (*session.SQLite)(nil)
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./session/ -run 'TestTouch|TestDelete|TestList|TestSQLiteIsAStore' -v`
Expected: FAIL — `store.TouchSession undefined`.

- [ ] **Step 3: Write the implementation**

Append to `session/sqlite.go`:

```go
// TouchSession rolls a session's activity and expiry forward. Callers rate-limit
// this with [Policy.NeedsTouch] so an authenticated request stays a read.
func (s *SQLite) TouchSession(ctx context.Context, id string, lastSeen, expires time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE auth_session SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		stamp(lastSeen), stamp(expires), id)
	if err != nil {
		return fmt.Errorf("session: touch: %w", err)
	}
	return nil
}

// DeleteSession revokes one session by its public id — the "sign out this
// device" path. Deleting a row that is already gone is success: logging out
// twice is not a failure.
func (s *SQLite) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_session WHERE id = ?`, id); err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	return nil
}

// DeleteSessionByToken is logout: the browser presents its cookie and the row
// behind it goes.
func (s *SQLite) DeleteSessionByToken(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM auth_session WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("session: delete by token: %w", err)
	}
	return nil
}

// ListSessions returns every row, expired ones included: the caller is a
// "your devices" screen, which must show a session in order to revoke it.
// Newest first, because that is the one the reader is currently using.
func (s *SQLite) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, selectColumns+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		sess, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("session: list: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./session/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add session/sqlite.go session/sqlite_test.go
git commit -m "feat(session): touch, revoke and list sessions"
```

---

### Task 5: The sweep, and proof the gate schedules it

**Files:**
- Modify: `session/sqlite.go`
- Modify: `session/sqlite_test.go`
- Create: `session/sweep_wiring_test.go`

**Interfaces:**
- Consumes: everything from Task 4.
- Produces: `(*SQLite).DeleteExpiredSessions(ctx, now time.Time) (int64, error)`. After this task `*SQLite` satisfies `session.ExpiredSweeper`, which is what makes `oidcauth.SweepJob` return `ok == true`.

- [ ] **Step 1: Write the failing tests**

Append to `session/sqlite_test.go`:

```go
// The sweep is housekeeping: it keeps the table bounded. It must remove exactly
// the dead rows and report how many, because the count is what the job logs.
func TestDeleteExpiredRemovesOnlyDeadRows(t *testing.T) {
	store, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	live := aSession()
	live.ExpiresAt = now.Add(time.Hour)
	dead := aSession()
	dead.ID, dead.ExpiresAt = "sess-dead", now.Add(-time.Hour)
	onTheLine := aSession()
	onTheLine.ID, onTheLine.ExpiresAt = "sess-edge", now

	for i, s := range []session.Session{live, dead, onTheLine} {
		if err := store.CreateSession(context.Background(), s, session.HashToken(string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}

	// A session expiring exactly now is spent, so it goes with the dead one.
	n, err := store.DeleteExpiredSessions(context.Background(), now)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("swept %d rows, want 2", n)
	}

	left, err := store.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != live.ID {
		t.Errorf("remaining = %+v, want only the live session", left)
	}
}
```

Create `session/sweep_wiring_test.go`. This lives in its own file because it imports `oidcauth`, which the rest of the package's tests deliberately do not:

```go
package session_test

import (
	"path/filepath"
	"testing"

	"database/sql"

	_ "modernc.org/sqlite"

	"github.com/neodata-io/neokit/oidcauth"
	"github.com/neodata-io/neokit/session"
)

// The store implementing ExpiredSweeper is what makes fiberauth schedule the
// sweep — SweepJob type-asserts for it and returns ok == false otherwise. That
// coupling is invisible at the call site, so it gets a test.
func TestTheGateWillScheduleTheSweepForThisStore(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := session.NewSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := oidcauth.SweepJob(store, nil); !ok {
		t.Error("SweepJob declined the store, so expired sessions would accumulate forever")
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./session/ -run 'TestDeleteExpired|TestTheGateWill' -v`
Expected: FAIL — `store.DeleteExpiredSessions undefined`.

- [ ] **Step 3: Write the implementation**

Append to `session/sqlite.go`:

```go
// DeleteExpiredSessions collects dead rows and reports how many went. A session
// expiring exactly at now is spent, so `<=` rather than `<`.
//
// This method is what makes the store an [ExpiredSweeper], which is what makes
// a gate schedule the sweep without being asked.
func (s *SQLite) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_session WHERE expires_at <= ?`, stamp(now))
	if err != nil {
		return 0, fmt.Errorf("session: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("session: sweep count: %w", err)
	}
	return n, nil
}
```

- [ ] **Step 4: Run the whole suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, all packages.

- [ ] **Step 5: Commit**

```bash
git add session/sqlite.go session/sqlite_test.go session/sweep_wiring_test.go
git commit -m "feat(session): sweep expired rows, so the gate schedules it"
```

---

### Task 6: A `Clock` interface — DROPPED, do not implement

**Dropped during execution.** `clock`'s package doc already refuses this
explicitly: *"There is deliberately no Clock interface here. The consumer
declares it — one method, on its own side, naming only what it uses. What
repeats across projects is not the interface but the fake."*

Both applications declaring their own one-method interface is that design
working, not a gap — and it is the Go convention besides. ok-stables' comment
about it is a factual note, not a complaint. The steps below are left in place so
the reversal is legible; **do not carry them out.**

~~Both consuming applications declared their own `interface{ Now() time.Time }` because `clock` exports only concrete types. ok-stables' code says so in a comment.~~

**Files:**
- Modify: `clock/clock.go`
- Modify: `clock/clock_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `clock.Clock` interface, satisfied by `clock.RealClock` and `*clock.Fake`.

- [ ] **Step 1: Write the failing test**

Append to `clock/clock_test.go`:

```go
// The interface is the point: a caller stores a Clock and a test swaps a Fake
// in. Without it every consumer declares its own one-method interface, which is
// what both applications using neokit ended up doing.
func TestBothClocksSatisfyTheInterface(t *testing.T) {
	var _ clock.Clock = clock.RealClock{}
	var _ clock.Clock = clock.NewFake(time.Unix(0, 0))
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./clock/ -run TestBothClocksSatisfy -v`
Expected: FAIL — `undefined: clock.Clock`.

- [ ] **Step 3: Write the implementation**

Add to `clock/clock.go`, above `RealClock`:

```go
// Clock is the time source a caller stores so a test can replace it. Exported
// as an interface because otherwise every consumer declares its own identical
// one-method interface to hold a [RealClock] or a [Fake].
type Clock interface{ Now() time.Time }
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./clock/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add clock/
git commit -m "feat(clock): export the Clock interface both consumers declared themselves"
```

---

### Task 7: Documentation

**Files:**
- Modify: `README.md:199` (the packages table)
- Modify: `oidcauth/fiberauth/gate.go` (the `Options.Sessions` doc)
- Modify: `oidcauth/fiberauth/sessions.go` (the trailing note about the sweep)

**Interfaces:**
- Consumes: `session.NewSQLite` from Task 2.
- Produces: nothing.

- [ ] **Step 1: Add the packages-table row**

In `README.md`, the row for `safe` `ids` `clock` currently reads:

```
| `safe` `ids` `clock` | goroutine recovery, id/token generation, injectable clock |
```

Insert a new row directly after the `oidcauth/fiberauth` row:

```
| `session` | `Session`, `Store`, `Policy` — and `NewSQLite`, the store neokit ships |
```

- [ ] **Step 2: Correct the two claims the README now contradicts**

`README.md` line 153 says `Sessions: store, // your own storage; neokit ships none`. Replace that line with:

```go
    Sessions:     session.NewSQLite(db),   // or your own storage
```

and in the same section replace the sentence *"A store that can prune in one statement is pruned daily"* — it stays true, no edit needed. Verify by reading the surrounding paragraph.

- [ ] **Step 3: Correct `fiberauth.Options.Sessions`**

In `oidcauth/fiberauth/gate.go`, the field doc reads:

```go
	// Sessions persists signed-in browsers. Required when Provider can return
	// non-nil.
	Sessions oidcauth.SessionStore
```

Replace with:

```go
	// Sessions persists signed-in browsers. Required when Provider can return
	// non-nil. [session.NewSQLite] is the store neokit ships; any
	// [oidcauth.SessionStore] does.
	Sessions oidcauth.SessionStore
```

- [ ] **Step 4: Correct the package doc**

`oidcauth/fiberauth/gate.go:8-9` says *"It stores nothing itself — you supply an [oidcauth.SessionStore] over whatever database you already have."* Replace with:

```go
// It is deliberately separate from oidcauth so the protocol half carries no web
// framework, and it is deliberately small: [Gate] mounts five routes, resolves an
// identity per request, and guards the ones you point at it. It stores nothing
// itself — pass [session.NewSQLite], or your own [oidcauth.SessionStore].
```

- [ ] **Step 5: Verify and commit**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

```bash
git add README.md oidcauth/fiberauth/
git commit -m "docs: neokit ships a session store now"
```

---

## Self-Review

**Spec coverage.** Every section maps to a task: the `session` package and the aliases → Task 1; `NewSQLite` and the schema → Task 2; the six `Store` methods → Tasks 3-4; `ExpiredSweeper` and the sweep wiring → Task 5; `clock.Clock` → Task 6; the "no boot-report line" decision needs no task, since not declaring is the absence of code. Every test named in the spec's Testing section appears: round trip (T3), raw token never stored (T3), unknown hash (T3), touch (T4), the two deletes (T4), list including expired (T4), sweep count (T5), idempotent DDL (T2), expiry enforced by `Policy.Live` not the store (T3), alias identity (T1).

**Placeholders.** None. Every code step carries the code.

**Type consistency.** `Store` (not `SessionStore`) throughout `session`; `SessionStore` only as the `oidcauth` alias. `scan`, `stamp`, `parse` and `selectColumns` are introduced in Task 3 and reused by name in Tasks 4-5. `NewSQLite` returns `(*SQLite, error)` at every mention.

**One risk flagged for the implementer.** Task 1 Step 4 deletes most of `oidcauth/identity.go`. Let `go build` name the unused imports rather than pruning them by eye.
