# Subsystem.Close, and stating the Fiber choice

Date: 2026-08-03
Status: approved, ready to plan

## Problem

Two things prompted this, one of which turned out not to need solving.

**Declaring a dependency takes two calls.** A service that opens a store writes
its name twice — once for teardown, once for the boot report and readiness:

```go
a.Shutdown.PushCloser("database", store)
a.Declare(app.Subsystem{Name: "database", On: true, Detail: cfg.DatabasePath, Ready: store.Ping})
```

The two strings can drift, and when they do the shutdown log and the boot report
disagree about what the thing is called. `app.Subsystem` already exists to stop
exactly that class of drift between the report and `/readyz`.

**The Fiber coupling is undocumented.** `App.HTTP` is a `*fiber.App` and handlers
are Fiber handlers, but nothing states that as a decision — a reader infers it
from a type signature, which reads like an accident waiting to be abstracted.

## Decisions taken, including what was rejected

The starting question was broader: whether to remove Fiber from neokit's public
surface entirely. Four directions were compared — a hand-written `web.Ctx` over
`net/http`, unbundling HTTP from neokit altogether, pure `net/http` handlers, and
keeping Fiber exposed. **Keeping Fiber exposed was chosen.** neokit is a
Fiber-based kit; the fix is to say so, not to hide it behind a wrapper that would
keep the dependency, add an abstraction, and still need an escape hatch.

Consequently there is **no `web` package**, `fiberx` and `oidcauth/fiberauth` keep
their names, and `App.HTTP` stays `*fiber.App`.

Two `Options` fields — `MapError` and `QuietPath`, to replace post-construction
assignment of `a.Errors.Mapper` and `a.Errors.QuietPath` — were designed and then
**rejected**. They cost the same line count at the call site, and because
`App.Errors` stays exported they would leave two ways to set one thing. The
existing design is deliberate and documented in two places:

- `fiberx/errors.go:27-31` — "configuring one of them at construction and the
  other by assignment would be an arbitrary split."
- NeoGate's `server/cmd/api/main.go`, under `app.New` — "set on the renderer
  neokit exposes rather than passed through Options."

Checked while investigating: `MetricsAndLogger` reads `e.QuietPath` per request
via `e.skipLog` (`fiberx/middleware.go:168`), so assignment after `New` takes
effect. There is no latent bug here.

## Design

### 1. `Subsystem` gains a `Close` field

`app/subsystem.go`:

```go
type Subsystem struct {
	Name   string
	On     bool
	Detail string
	Ready  func(ctx context.Context) error

	// Close releases whatever this subsystem holds. Pushed onto App.Shutdown at
	// Declare time, so it unwinds with the caller's own steps.
	Close func(ctx context.Context) error
}
```

`Declare` gains three lines:

```go
func (a *App) Declare(s Subsystem) {
	a.subsystems = append(a.subsystems, s)

	if s.On && s.Ready != nil {
		a.health.Register(s.Name, s.Ready)
	}
	if s.Close != nil {
		a.Shutdown.Push(s.Name, s.Close)
	}
}
```

### 2. `lifecycle.Closer`

`lifecycle/lifecycle.go`, mirroring the existing `Stack.PushCloser`:

```go
// Closer adapts an io.Closer to a Step, for a dependency whose Close takes no
// context.
func Closer(c interface{ Close() error }) Step {
	return func(context.Context) error { return c.Close() }
}
```

### 3. State the Fiber choice

A short paragraph near the top of the `app` package doc in `app/app.go`:

```go
// neokit builds on Fiber v3. App.HTTP is a *fiber.App and handlers are ordinary
// Fiber handlers — a deliberate choice, not an implementation detail waiting to
// be abstracted away.
```

The same statement as a line near the top of `README.md`, next to the existing
quick-start example.

## Behaviour rules

These are the parts a reader would otherwise have to derive, so they are stated
and each is pinned by a test:

1. **`Close` runs whenever it is non-nil, including when `On` is false** — unlike
   `Ready`, which is ignored when the subsystem is off. A nil `Close` already
   means "nothing to release"; skipping a non-nil one because the feature reports
   off would leak whatever the caller allocated.
2. **`Close` is pushed at `Declare` time**, the same point `PushCloser` was called
   from, so the stack's LIFO unwinding order is unchanged: `Run`'s own steps
   (streams, api, background-context) are pushed later and therefore run first,
   then the caller's steps in reverse, then metrics-export and tracing.
3. **`Shutdown.Push` and `PushCloser` stay** and are unchanged. Not every teardown
   step is a subsystem — NeoGate's `plugins` step is not declared — and those keep
   using the stack directly.
4. neokit's own declarations (tracing, metrics export, metrics endpoint, health)
   set no `Close`, so nothing about the existing boot sequence changes.

## Non-goals

- No `web` package, no renaming of `fiberx` or `oidcauth/fiberauth`.
- No `Options.MapError` or `Options.QuietPath`.
- No change to `App.HTTP`'s type, the middleware chain, or the boot order.
- No `app.Component` type and no `Add` method; `Declare` is extended instead.
- No NeoGate changes are required by this — adoption is opt-in.

## Testing

New tests in `app/` and `lifecycle/`:

- `TestSubsystemCloseRunsOnShutdown` — a declared `Close` runs when `App.Close`
  does.
- `TestSubsystemCloseRunsWhenTheSubsystemIsOff` — rule 1.
- `TestSubsystemCloseUnwindsInReverseDeclarationOrder` — two subsystems with
  `Close`, asserted LIFO.
- `TestSubsystemWithoutCloseDoesNotTouchTheStack` — a `Subsystem` with a nil
  `Close` pushes nothing, asserted via `Stack.Len`.
- `TestCloserAdaptsAnIoCloser` — `lifecycle.Closer` forwards the error and
  ignores the context.

Existing tests are untouched. This change is additive; if any existing test
breaks, that is the signal that it was not.

## NeoGate impact

Opt-in and mechanical where taken up. `server/cmd/api/main.go` collapses:

```go
a.Declare(app.Subsystem{
	Name: "database", On: true, Detail: cfg.DatabasePath,
	Ready: store.Ping,
	Close: lifecycle.Closer(store),
})
```

The `plugins` step stays a plain `a.Shutdown.Push` — it is not a declared
subsystem, and making it one is a separate decision.

## Verification

`go build ./... && go vet ./... && go test ./...` from the repo root, plus
`go test ./app/... ./lifecycle/...` for the changed packages specifically.

## Risks

Low. The change is three lines of behaviour behind a nil check, one three-line
adapter, and documentation. The one judgement call a reviewer should look at is
rule 1 — `Close` running while `On` is false — which is deliberate and tested.

## Prerequisite

The working tree has staged *and* unstaged changes in flight across `app/`,
`health/`, `metrics/`, `config/`, `go.mod` and `README.md`. Land those first, or
this change lands inside an unreviewable diff.

## Style note

New doc comments are 1-3 lines rather than matching the surrounding paragraph
style.
