# A session store neokit ships

Date: 2026-08-03
Status: draft, awaiting review
Related: `2026-08-03-component-and-self-declaring-constructors-design.md`

## Problem

`oidcauth.SessionStore` is an interface neokit defines and ships no
implementation of. Its own doc explains why: *"this package ships none, so it
never forces a second database or a schema you did not choose."*

That reasoning holds for the storage *engine*. What it produced is a
batteries-included layer that hands back homework. `neokit.App.Login` is
documented as one call that enables the feature, and it cannot be called at all
until the caller has written six methods, a schema and a migration.

Two applications have now paid that cost independently.

**NeoGate** wrote `adapter/sqlite/session.go` — 129 lines plus tests — over an
`auth_session` table: random token in the cookie, only its SHA-256 in the
database, expiry enforced on read, revocation by row.

**ok-stables** wrote `app/adminauth/service.go` for a password login with no OIDC
anywhere. It arrived at the same design without sharing a line: mint 32 random
bytes, store only `sha256`, stamp a TTL, revoke by `DELETE`.

One design, invented twice, shared nowhere. That is the definition of something
that belongs in the framework.

### The dependency that blocks the obvious fix

`Session`, `SessionStore`, `Policy` and `HashToken` live in `oidcauth`, which
imports `go-oidc` and `oauth2`. ok-stables authenticates one admin with a
password and speaks no OIDC at all. Implementing `oidcauth.SessionStore` would
make it link an OpenID Connect relying party in order to store rows in a table.

That contradicts the rule the README states outright: *"Every package is
independent: importing one never drags in another's dependencies. A binary that
does not import `oidcauth` links neither `go-oidc` nor `oauth2`."*

So the store cannot simply be added to `oidcauth`. The types have to be
reachable without it.

## Decisions taken

- **A new package `session`**, importing only the standard library (`context`,
  `time`, `crypto/sha256`, `crypto/rand`, `database/sql`, `encoding/json`). It
  holds the vocabulary — `Session`, `Identity`, `Store`, `ExpiredSweeper`,
  `Policy`, `HashToken`, `RandomToken` — and one SQLite-backed implementation of
  `Store`.

- **`oidcauth` keeps every one of those names as a type alias.** `oidcauth.Session`
  stays `oidcauth.Session`. Aliases, not copies, so NeoGate's existing store
  satisfies the interface unchanged the day it upgrades.

- **The store carries no clock, because it does not need one.** Every time value
  it writes arrives as an argument: `CreateSession` takes a stamped `Session`,
  `TouchSession` takes both instants, `DeleteExpiredSessions` takes `now`. The
  one place a clock would be used is filtering expired rows on read — and
  `SessionStore`'s own doc says an implementation *may* do that but is not
  required to, because *"expiry is re-checked by the middleware, so the security
  property does not depend on which store is wired in."* Not filtering keeps the
  constructor a single argument and keeps the security property in the one place
  that is already tested for it.

- **The table is `auth_session`**, created with `CREATE TABLE IF NOT EXISTS` and
  its indexes at construction. Times are RFC3339 in UTC.

- **No boot-report line of its own.** `fiberauth.New` already declares `login`
  and hangs the expired-session sweep off that same line, on the stated rule of
  one feature, one name. A store that appeared separately would be the second
  name for the same feature.

- **`clock.Clock` interface**, four lines. Both applications declared their own
  `interface{ Now() time.Time }`; ok-stables' code carries a comment saying it
  had to, *"because neokit's clock package exports the concrete RealClock but no
  interface of its own."*

### Rejected, with reasons

**Putting it in `sqlitex`.** That package is engine helpers — open, migrate,
query, snapshot — and its value is having no opinion about tables.
`sqlitex.Sessions` would put an application schema inside the package whose job
is to be schema-agnostic.

**A separate `sessionsqlite` package.** `database/sql` is standard library and
the store never imports a driver, so it costs an importer nothing beyond what
`session` already costs. A second package would buy a longer import path and
nothing else.

**Creating the table through `sqlitex.Migrate`.** It versions with
`PRAGMA user_version` over an append-only slice the *application* owns. A
neokit-owned step inserted into that sequence shifts the version of every later
step in every database already in the field — which `sqlitex`'s own doc calls
the failure the append-only rule exists to prevent. `CREATE TABLE IF NOT EXISTS`
is idempotent and touches no counter.

**A configurable table name.** One option, two code paths, and the possibility of
the store and the sweep disagreeing about which table they mean.

**Filtering expired rows inside the store.** It reads as defence in depth and is
really a second place for the expiry rule to live, which is how the two come to
disagree. The middleware's `Policy.Live` is the single enforcement point and has
tests pinning it.

**Moving the types out of `oidcauth` without aliases.** Correct structure, needless
breakage: NeoGate is a live consumer on v0.7.0.

## The API

```go
store, err := session.NewSQLite(db)   // creates auth_session and its indexes
if err != nil {
    return err
}
gate := a.Login(fiberauth.Options{Sessions: store, CookiePrefix: "myapp"})
```

Two lines replace NeoGate's 129 and the storage half of ok-stables' `adminauth`.

```go
package session

// Session is a signed-in browser. The cookie's token is deliberately absent —
// only its SHA-256 is stored, so a leaked database yields nothing replayable.
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

func (s Session) Identity() Identity

// Identity is the authenticated principal. A zero value means not authenticated.
type Identity struct {
    Subject string
    Name    string
    Email   string
    Groups  []string
    Owner   bool
}

// Store persists sessions. tokenHash is always the output of HashToken.
type Store interface {
    CreateSession(ctx context.Context, s Session, tokenHash string) error
    SessionByToken(ctx context.Context, tokenHash string) (Session, bool, error)
    TouchSession(ctx context.Context, id string, lastSeen, expires time.Time) error
    DeleteSession(ctx context.Context, id string) error
    DeleteSessionByToken(ctx context.Context, tokenHash string) error
    ListSessions(ctx context.Context) ([]Session, error)
}

// ExpiredSweeper is the optional companion: bulk collection of dead rows.
type ExpiredSweeper interface {
    DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}

// Policy is the session lifetime rules. See DefaultPolicy.
type Policy struct {
    TTL           time.Duration
    MaxLifetime   time.Duration
    TouchInterval time.Duration
}

func DefaultPolicy() Policy
func (p Policy) ExpiryFor(createdAt, now time.Time) time.Time
func (p Policy) Live(s Session, now time.Time) bool
func (p Policy) NeedsTouch(s Session, now time.Time) bool

// SQLite is a Store over any *sql.DB speaking SQLite. It implements
// ExpiredSweeper, so fiberauth schedules the sweep without being asked.
type SQLite struct{ /* unexported */ }

func NewSQLite(db *sql.DB) (*SQLite, error)

func HashToken(token string) string
func RandomToken(n int) (string, error)
```

`Policy`, `HashToken` and `RandomToken` move from `oidcauth` unchanged; the
aliases left behind keep every existing call site compiling.

## Schema

```sql
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
```

`token_hash UNIQUE` provides the lookup index for the hot path; the second index
serves the sweep's ranged `DELETE`.

The column names and the RFC3339 encoding are NeoGate's, deliberately. Its
existing rows already fit this table, so adopting the package is a deletion with
no data migration behind it.

`Groups` is JSON in one column rather than a join table: it is a snapshot taken
at login, never queried by membership, and rewritten whole or not at all.

## What the applications gain

| | Today | After |
| --- | --- | --- |
| NeoGate | 129 lines + tests + a migration step | one call |
| ok-stables | its own store, its own table, no per-device revoke | one call, and revoke-one-device it does not have |
| A new app | six methods before anyone can log in | one call |

ok-stables also stops needing a flat 90-day timer: `Policy` rolls a session
forward while it is used and caps its total age, which is the behaviour its own
comment says it chose the long TTL to avoid needing.

## Testing

The suite runs against a real in-memory SQLite database, since the whole package
is SQL.

- Round trip: create, then resolve by token hash.
- **The raw token never reaches the database.** Create a session, then scan every
  column of every row for the token as a substring. This is the property the
  design exists for and the only test whose failure is a security bug.
- An unknown hash resolves to `(zero, false, nil)` — absent is not an error.
- `TouchSession` moves `last_seen_at` and `expires_at` and nothing else.
- `DeleteSession` and `DeleteSessionByToken` each remove exactly one row.
- `ListSessions` returns every row, including expired ones, because the caller is
  a "your devices" screen that must show a session in order to revoke it.
- `DeleteExpiredSessions` removes only rows at or past the given instant and
  reports the count it removed.
- `NewSQLite` twice against one database succeeds — the DDL is idempotent, which
  is what lets it run on every boot.
- A session the store returns past its expiry is still rejected by `Policy.Live`,
  pinning that expiry is enforced in the middleware rather than in the store.
- `oidcauth.Session` and `session.Session` are the same type, by assignment in a
  compile-time test — the alias is the compatibility guarantee and nothing else
  proves it.

## Out of scope

Password verification, login attempt throttling, API tokens, proxy-header
identity and a Postgres store. Each is its own spec. This one ships the row.
