# Component, and constructors that declare themselves

Date: 2026-08-03
Status: approved, ready to plan

## Problem

Two complaints, one root cause.

**"Subsystem is vague."** It is, because it names two different things at once: an
external dependency that can *break* (a database), and a feature you built that is
*on or off* (login). One word over two meanings is hard to teach.

**"I don't want to declare each time — I just want to enable a database."** Today
every dependency is built and then described separately:

```go
store, err := sqlite.OpenStore(cfg.DatabasePath)
if err != nil { return err }
a.Declare(app.Subsystem{
	Name: "database", On: true, Detail: cfg.DatabasePath,
	Ready: store.Ping, Close: lifecycle.Closer(store),
})
```

neokit already ships `sqlitex` and `oidcauth/fiberauth`. Each knows its own health
check, its own cleanup and its own routes. Neither should have to ask.

## Decisions taken

- **`Subsystem` is renamed `Component`.** Chosen over `Service` (which already
  means the whole process) and over splitting into `Dependency` + `Feature` (two
  concepts to learn).
- **Constructors take the app as their first argument**, and registering happens
  by constructing. No `Enable` verb — calling it *is* enabling it.
- **The app argument is required.** One function per package, no standalone
  variant, no nil-means-skip. Pre-1.0, so the breaking change is allowed
  (`README.md`: "the API may change between `v0.x` releases").
- **The type lives in a new leaf package, `declare`.** Named for the verb rather
  than the noun because `component.Component` stutters, which Go style avoids.
- **Fiber-coupled packages take `*app.App`; light ones take `declare.Declarer`.**
- **A constructor takes a component name when you could plausibly have two.**
  `sqlitex` does. `fiberauth` does not — a service has one login gate — and
  hardcodes `"login"`.
- **Two packages this round: `sqlitex` and `fiberauth`.** They carry real wiring —
  health checks, cleanup, route registration, a scheduled job.

### Rejected, with reasons

**Functional options** (`app.New(WithDatabase(), WithLogin())`), the shape used by
`neodata-io/neodata-go`. An `Option` returns an `Option`, not a `*sql.DB`, so the
constructed thing has to be stored and fetched later — which forces either a
service container or an exported field on `App` per feature. neodata-go takes the
container route: `NeoCtx` holds `db`, `messaging`, `httpServer` privately behind
`GetDB()`, plus a `ServiceRegistry` keyed by string over `interface{}`. That turns
a startup mistake into a runtime error in every handler, and `app`'s own doc
already rules it out: *"deliberately not a container… no lookup, no reflection."*

**Wrappers on `App`** (`a.EnableDatabase(...)`). Would make `app` import SQLite,
OIDC and every other driver, so every service compiles all of them. Breaks
`README.md`'s rule: *"Nothing reaches a binary unless its package is imported."*

**Converting `backup` and `notify`.** Neither has a `Close` or a health check —
`backup.Service` is `Backup`/`WriteDaily`/`List`, and pinging ntfy at boot would
be wrong. Their components would be report-only, so a self-declaring constructor
saves one `Declare` line and costs a permanent declarer and name parameter.
`notify.Sender` also already requires `Name() string`, so a component name would
be a second name on the same object. They stay as they are.

## What we took from neodata-go

Worth copying: the one-line-per-feature reading style; `type Option func(*T) error`
so setup can fail in one place; and the "already configured, skip with a warning"
guard in `WithNATS`, which matches the duplicate check merged in `f9003b0`.

Not copying: `ServiceRegistry`, the private-field-plus-getter access, and the
`func(...) (interface{}, error)` handler signature.

Capability gaps it revealed, **out of scope here** but worth their own specs later:
authorization (it has Casbin `CanUserPerformAction`; neokit has authentication via
`oidcauth` and nothing beyond `RequireOwner`), a shared cache (ours is in-memory
only), and a real broker (our `pubsub` cannot cross instances).

## Design

### 1. The `declare` package

Feature packages must not import `app` — that would pull Fiber, OpenTelemetry and
Prometheus into anything using `sqlitex`. So the type and the interface live in a
package that imports only `context`:

```go
package declare // github.com/neodata-io/neokit/declare

// Component is one part of a service: whether it is on, a line for the boot
// report, an optional readiness check and an optional teardown step.
type Component struct {
	Name   string
	On     bool
	Detail string
	Ready  func(ctx context.Context) error
	Close  func(ctx context.Context) error
}

// Declarer is what a component registers itself with. *app.App satisfies it.
type Declarer interface{ Declare(Component) }
```

`app` keeps the friendly spelling with an alias, so `app.Component` is what
consumers write and nothing changes for anyone declaring by hand:

```go
type Component = declare.Component
```

### 2. Two constructor shapes, by dependency weight

A package that is already Fiber-coupled loses nothing by importing `app`. A light
package must not.

| Package | Takes | Why |
|---|---|---|
| `sqlitex` | `declare.Declarer` | stays light — no Fiber, no OTEL |
| `fiberauth` | `*app.App` | already imports Fiber; needs `a.HTTP` to register routes |

The call site is identical either way, because `*app.App` satisfies `Declarer`.

### 3. Per-package signatures

**`sqlitex`** — `Open` gains the declarer and a name:

```go
func Open(d declare.Declarer, name, path string, migrate func(*sql.DB) error) (*sql.DB, error)
```

Declares `On: true`, `Detail: path`, `Ready: db.PingContext`, `Close: db.Close`.
The name is a parameter so a service can open two databases without colliding.

**`fiberauth`** — `New` gains the app and absorbs `Register`:

```go
func New(a *app.App, o Options) *Gate
```

Declares `Name: "login"`, `On: gate.Enabled()`, and a `Detail` naming the issuer
when configured or `"not configured"` when not. Registers the handshake routes on
`a.HTTP`.

The session sweep is **not** started by `New`. `jobs.Job` has `Run` and `Start`
but no `Stop` — a job runs until the context it was started with is cancelled — so
starting one inside a constructor would be a background goroutine the caller can
neither observe nor decline. `SweepJob` keeps its current caller-started shape.

No error return: `New` cannot fail today and route registration cannot either, so
an always-nil error would be a lie. `Register(*fiber.App)` is removed — `New` does
it. `Options` already carries `APIBase` and `HandshakeBase`, so callers keep
control of where the routes mount; no new field is needed.

### 4. The resulting `main.go`

```go
a, err := app.New(app.Options{Name: "neogate", Version: buildinfo.Get().Version, Base: cfg.Base})
if err != nil { return err }
defer a.Close()

db, err := sqlitex.Open(a, "database", cfg.DatabasePath, migrate)
if err != nil { return err }

gate := fiberauth.New(a, fiberauth.Options{Sessions: store, CookiePrefix: "neogate"})

return a.Run()
```

`db` and `gate` are ordinary values passed to your own constructors — a forgotten
dependency is a compile error, not a runtime one.

## Non-goals

- No functional options, no service registry, no getters on `App`.
- No change to `App.HTTP`'s type or to the middleware chain.
- `app.Declare` stays public and unchanged in behaviour — declaring by hand
  remains the way to register anything neokit does not ship.
- `backup` and `notify` keep their current signatures; declare them by hand.
- No authorization, shared cache or broker package. Separate specs.
- `cache`, `pubsub`, `webpush`, `ids`, `clock`, `safe`, `httpc`, `logx`, `netx`
  keep their current signatures. They are tools, not components: nothing to
  report, check or close.

## Testing

**`sqlitex`**: `Open` registers exactly one component whose `Name`, `On` and
`Detail` match what the boot report should show; its `Ready` reaches the real
database and fails once the database is closed; its `Close` is registered and
closes the handle. Two `Open` calls with different names produce two components
and no duplicate-name warning.

**`fiberauth`**: `New` registers one component named `"login"` whose `On` follows
`Gate.Enabled()` and whose `Detail` is the provider's issuer when configured; the
handshake routes answer after `New` with no separate `Register` call.

**`declare`**: `Component`'s zero value is inert. That `*app.App` satisfies
`Declarer` is asserted at compile time with
`var _ declare.Declarer = (*app.App)(nil)` in an `app` test, since `declare`
cannot import `app`.

Existing tests must keep passing except where a signature changed; those are
updated mechanically, not rewritten.

## Migration

**neokit:** new `declare` package; `app.Subsystem` → `app.Component` alias with
its four internal declarations updated; two package signatures changed; the
`fiberauth` README example updated to drop `Register`.

**NeoGate:** `sqlitex` (5 files) and `oidcauth` (11 files), plus the `Declare`
calls in `server/cmd/api/main.go`. Every break is a compile error.

## Risks

**`fiberauth.New` absorbing `Register`** is the most hidden behaviour in the
design: one call now builds the gate, declares it, mounts routes and schedules a
job. `Options.APIBase`/`HandshakeBase` keep the mount points controllable, but a
caller who wanted to register routes conditionally, or on a sub-router, loses
that. If that matters, `Register` comes back as an option rather than a method.

**The rename touches every `Subsystem` reference.** Mitigated by the alias:
`app.Subsystem` could be kept as a deprecated alias alongside `app.Component` if
the NeoGate migration proves noisy, though the plan does not do this.

## Style note

New doc comments are 1-3 lines, not the paragraph style of the surrounding code.
