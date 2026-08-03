# Component and Self-Declaring Constructors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rename `Subsystem` to `Component`, move it to a dependency-free `declare` package, and make `sqlitex.Open` and `fiberauth.New` register themselves so callers never write `Declare` for them.

**Architecture:** A new leaf package `declare` holds `Component` and a one-method `Declarer` interface, importing only `context`. `app.Component` is a type alias, so `*app.App` satisfies `Declarer` without any package importing `app` that shouldn't. `sqlitex` takes `declare.Declarer` and stays light; `fiberauth` takes `*app.App` because it already imports Fiber and needs `a.HTTP` to mount routes.

**Tech Stack:** Go 1.25.0, module `github.com/neodata-io/neokit`. Standard library `testing` only — these suites use no assertion library.

## Global Constraints

- **`declare` must import only `context`.** If it ever imports `app`, the whole design is void — `sqlitex` would pull Fiber, OpenTelemetry and Prometheus into every binary. `README.md`'s rule: *"Nothing reaches a binary unless its package is imported."*
- **`app.Declare` keeps its current behaviour**, including the empty-name panic and the duplicate/late warnings added in `f9003b0`.
- **`fiberauth.New` returns `*Gate`, not `(*Gate, error)`.** It has no failure path; an always-nil error would be a lie.
- **New doc comments are 1-3 lines**, not the paragraph style of the surrounding code.
- **`backup` and `notify` are not touched.** They have no `Close` and no health check, so their components would be report-only.
- Spec: `docs/superpowers/specs/2026-08-03-component-and-self-declaring-constructors-design.md`

**Baseline check.** Run before Task 1:

```bash
git status --short && go test ./... 2>&1 | grep -vE '^\?|^ok' || echo CLEAN
```

Expected: no output from `git status` except untracked `docs/`, and `CLEAN`.

---

### Task 1: The `declare` package and the `Component` rename

**Files:**
- Create: `declare/declare.go`, `declare/declare_test.go`
- Rename: `app/subsystem.go` → `app/component.go`, `app/subsystem_internal_test.go` → `app/component_internal_test.go`, `app/subsystem_close_test.go` → `app/component_close_test.go`
- Modify: `app/app.go`, `app/app_test.go`, `app/declare_validation_test.go`, `app/run_test.go`, `examples/production-service/main.go`, `README.md`

**Interfaces:**
- Produces: `declare.Component` (struct with `Name string`, `On bool`, `Detail string`, `Ready func(context.Context) error`, `Close func(context.Context) error`); `declare.Declarer` (interface with `Declare(Component)`); `app.Component` as `= declare.Component`; `(*app.App).Declare(declare.Component)` and `(*app.App).Components() []declare.Component`

There are 69 `Subsystem` references across 8 Go files plus 2 in `README.md`. The rename is mechanical and done with `sed`; the interesting work is the new package and the alias.

- [ ] **Step 1: Write the failing test for `declare`**

Create `declare/declare_test.go`:

```go
package declare_test

import (
	"context"
	"errors"
	"testing"

	"github.com/neodata-io/neokit/declare"
)

// recorder is the smallest possible Declarer. It is also the shape every feature
// package's tests use, so it doubles as the worked example.
type recorder struct{ got []declare.Component }

func (r *recorder) Declare(c declare.Component) { r.got = append(r.got, c) }

// The interface has to be satisfiable without importing app — that is the entire
// reason this package exists.
func TestDeclarerIsSatisfiableWithoutApp(t *testing.T) {
	var d declare.Declarer = &recorder{}

	want := errors.New("down")
	d.Declare(declare.Component{
		Name: "database", On: true, Detail: "./data/app.db",
		Ready: func(context.Context) error { return want },
	})

	r := d.(*recorder)
	if len(r.got) != 1 {
		t.Fatalf("declared %d components, want 1", len(r.got))
	}
	got := r.got[0]
	if got.Name != "database" || !got.On || got.Detail != "./data/app.db" {
		t.Errorf("Component = %+v", got)
	}
	if err := got.Ready(context.Background()); !errors.Is(err, want) {
		t.Errorf("Ready err = %v, want %v", err, want)
	}
}

// The zero value must be inert: a caller that fills only Name and On has no
// Ready and no Close, and neither may be called.
func TestZeroComponentHasNoFuncs(t *testing.T) {
	var c declare.Component
	if c.Ready != nil || c.Close != nil {
		t.Error("zero Component must have nil Ready and Close")
	}
	if c.On {
		t.Error("zero Component must be off")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./declare/ 2>&1 | head -5
```

Expected: FAIL — `no required module provides package github.com/neodata-io/neokit/declare` or `no Go files in .../declare`.

- [ ] **Step 3: Create the package**

Create `declare/declare.go`:

```go
// Package declare holds the vocabulary a neokit component uses to register
// itself, and nothing else. It imports only context so that a light package —
// sqlitex, say — can take a Declarer without pulling in Fiber, OpenTelemetry and
// Prometheus along with app.
package declare

import "context"

// Component is one part of a service: whether it is on, a line for the boot
// report, an optional readiness check and an optional teardown step.
type Component struct {
	// Name is the label in the boot report, the readiness check's name and the
	// shutdown step's name. Required.
	Name string

	// On reports whether this component is configured and active.
	On bool

	// Detail is one line of context: a path, an issuer, a schedule, or the
	// reason it is off.
	Detail string

	// Ready, when set on an On component, registers a readiness check.
	Ready func(ctx context.Context) error

	// Close releases what this component holds. It runs even when On is false —
	// a non-nil Close means something was allocated and would otherwise leak.
	Close func(ctx context.Context) error
}

// Declarer is what a component registers itself with. *app.App satisfies it, so
// a constructor takes a Declarer rather than the app itself.
type Declarer interface{ Declare(Component) }
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./declare/ -v 2>&1 | grep -E '^(=== RUN|--- (PASS|FAIL)|ok|FAIL)'
```

Expected: PASS for `TestDeclarerIsSatisfiableWithoutApp` and `TestZeroComponentHasNoFuncs`.

- [ ] **Step 5: Rename Subsystem to Component across app and examples**

macOS `sed` needs the empty `-i ''`. This catches both cases, so `otelSubsystem` becomes `otelComponent`, the `subsystems` field becomes `components`, and the warning strings say "component":

```bash
grep -rl 'ubsystem' --include='*.go' app/ examples/ | xargs sed -i '' -e 's/Subsystem/Component/g' -e 's/subsystem/component/g'
sed -i '' -e 's/Subsystem/Component/g' -e 's/subsystem/component/g' README.md
gofmt -w app/ examples/
```

- [ ] **Step 6: Rename the files to match**

```bash
git mv app/subsystem.go app/component.go
git mv app/subsystem_internal_test.go app/component_internal_test.go
git mv app/subsystem_close_test.go app/component_close_test.go
```

- [ ] **Step 7: Point app at the declare package**

In `app/component.go`, delete the local `Component` struct definition (the whole `type Component struct { ... }` block that `sed` just renamed) and replace it with an alias. Keep the doc comment above it:

```go
// Component is one part of the process: whether it is on, a line for the boot
// report, a readiness check when it has a dependency worth probing, and a
// teardown step when it holds something.
//
// One declaration produces all of them, so a component is named once and cannot
// appear in the report under one name and in /readyz or the shutdown log under
// another.
//
// An alias, so a package that must stay light — see [declare] — can accept one
// without importing app.
type Component = declare.Component
```

Add the import to `app/component.go`:

```go
import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/neodata-io/neokit/declare"
)
```

- [ ] **Step 8: Assert the interface is satisfied, from the app side**

`declare` cannot import `app`, so the compile-time check lives here. Append to `app/component_internal_test.go`:

```go
// The whole point of the declare package: a constructor can take a Declarer and
// be handed an *App. Breaking this breaks every self-declaring constructor.
var _ declare.Declarer = (*App)(nil)
```

Add `"github.com/neodata-io/neokit/declare"` to that file's imports.

- [ ] **Step 9: Run the full suite**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | grep -vE '^\?|^ok' || echo "FULL SUITE GREEN"
```

Expected: `FULL SUITE GREEN`. If `context` is now an unused import in `app/component.go`, remove it — the alias may have been its only user.

- [ ] **Step 10: Commit**

```bash
git add -A declare/ app/ examples/ README.md
git commit -m "refactor(app)!: rename Subsystem to Component, move it to declare

Subsystem named two things at once - an external dependency that can break and
a feature that is on or off - which made it hard to teach. Component covers both
without sounding abstract.

The type moves to a new leaf package importing only context, so a light package
like sqlitex can accept a Declarer without pulling Fiber, OpenTelemetry and
Prometheus in behind app. app.Component is an alias, so declaring by hand is
unchanged apart from the name."
```

---

### Task 2: `sqlitex.Open` declares itself

**Files:**
- Modify: `sqlitex/open.go:16-20` (doc comment and signature)
- Test: `sqlitex/open_test.go` (add tests, update call sites), plus call-site updates in `sqlitex/migrate_test.go`, `sqlitex/query_test.go`, `sqlitex/rollback_test.go`, `sqlitex/snapshot_test.go`

**Interfaces:**
- Consumes: `declare.Component`, `declare.Declarer` from Task 1
- Produces: `sqlitex.Open(d declare.Declarer, name, path string, migrate func(*sql.DB) error) (*sql.DB, error)`

There are 13 existing `Open(` call sites across five test files. All gain two arguments.

- [ ] **Step 1: Write the failing tests**

Append to `sqlitex/open_test.go`:

```go
// recorder captures what Open declares.
type recorder struct{ got []declare.Component }

func (r *recorder) Declare(c declare.Component) { r.got = append(r.got, c) }

// Open registers the database so a caller never writes Declare for it. The
// report line, the readiness check and the teardown all come from this one call.
func TestOpenDeclaresTheDatabase(t *testing.T) {
	var r recorder
	db, err := Open(&r, "database", filepath.Join(t.TempDir(), "app.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if len(r.got) != 1 {
		t.Fatalf("declared %d components, want 1", len(r.got))
	}
	c := r.got[0]
	if c.Name != "database" {
		t.Errorf("Name = %q, want %q", c.Name, "database")
	}
	if !c.On {
		t.Error("an opened database must be On")
	}
	if c.Ready == nil || c.Close == nil {
		t.Fatal("an opened database must declare both Ready and Close")
	}
	if err := c.Ready(context.Background()); err != nil {
		t.Errorf("Ready on a live database = %v, want nil", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := c.Ready(context.Background()); err == nil {
		t.Error("Ready must fail once the database is closed")
	}
}

// Detail is what an operator reads in the boot report to know which file this
// process actually opened.
func TestOpenDeclaresThePathAsDetail(t *testing.T) {
	var r recorder
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := Open(&r, "database", path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if r.got[0].Detail != path {
		t.Errorf("Detail = %q, want the path %q", r.got[0].Detail, path)
	}
}

// The name is a parameter precisely so a service can open two databases. They
// must not collide, or the boot report and /readyz list one of them twice.
func TestTwoDatabasesDeclareDistinctNames(t *testing.T) {
	var r recorder
	dir := t.TempDir()
	for _, name := range []string{"database", "analytics"} {
		db, err := Open(&r, name, filepath.Join(dir, name+".db"), nil)
		if err != nil {
			t.Fatalf("Open %s: %v", name, err)
		}
		t.Cleanup(func() { _ = db.Close() })
	}

	if len(r.got) != 2 {
		t.Fatalf("declared %d components, want 2", len(r.got))
	}
	if r.got[0].Name == r.got[1].Name {
		t.Errorf("both components named %q; the name parameter was ignored", r.got[0].Name)
	}
}

// A failed open must declare nothing: a component in the report for a database
// that was never opened is worse than no line at all.
func TestAFailedOpenDeclaresNothing(t *testing.T) {
	var r recorder
	if _, err := Open(&r, "database", "", nil); err == nil {
		t.Fatal("Open must reject an empty path")
	}
	if len(r.got) != 0 {
		t.Errorf("declared %d components after a failed open, want 0", len(r.got))
	}
}
```

Ensure `sqlitex/open_test.go` imports `context`, `path/filepath`, `testing` and `github.com/neodata-io/neokit/declare`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./sqlitex/ -run 'TestOpenDeclares|TestTwoDatabases|TestAFailedOpen' 2>&1 | head -5
```

Expected: FAIL to compile — `not enough arguments in call to Open`.

- [ ] **Step 3: Change the signature and declare**

In `sqlitex/open.go`, replace the doc comment and signature (currently lines 16-20):

```go
// Open opens a SQLite database with the settings a server actually wants, runs
// migrate against it, and declares it on d — so the boot report, the readiness
// check and the shutdown step all come from this one call. A nil migrate skips
// the migration step.
//
// name labels the component; pass distinct names when opening more than one
// database. Nothing is declared if the open fails.
func Open(d declare.Declarer, name, path string, migrate func(*sql.DB) error) (*sql.DB, error) {
```

Add the import:

```go
	"github.com/neodata-io/neokit/declare"
```

Then, immediately before the function's final successful `return db, nil`, add the declaration:

```go
	d.Declare(declare.Component{
		Name: name, On: true, Detail: path,
		Ready: db.PingContext,
		Close: func(context.Context) error { return db.Close() },
	})
	return db, nil
```

Add `"context"` to the imports if it is not already there.

- [ ] **Step 4: Update the 13 existing call sites**

Every existing `Open(` in the test files gains a no-op declarer and a name. Find them:

```bash
grep -rn 'Open(' sqlitex/*_test.go
```

Change each `Open(path, migrate)` to `Open(&recorder{}, "database", path, migrate)`. `recorder` is defined in `open_test.go` from Step 1 and is visible to every file in the package.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./sqlitex/ -v 2>&1 | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'
```

Expected: PASS for every test, including the four new ones.

- [ ] **Step 6: Run the full suite**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | grep -vE '^\?|^ok' || echo "FULL SUITE GREEN"
```

Expected: `FULL SUITE GREEN`.

- [ ] **Step 7: Commit**

```bash
git add sqlitex/
git commit -m "feat(sqlitex)!: Open declares the database itself

Open now takes a declare.Declarer and a component name, and registers the boot
report line, the readiness check and the shutdown step. A caller passes its
*app.App and never writes Declare for a database again.

The name is a parameter so a service can open two databases without their
components colliding. A failed open declares nothing."
```

---

### Task 3: `fiberauth.New` declares itself and mounts its routes

**Files:**
- Modify: `oidcauth/fiberauth/gate.go:157` (`New`), `oidcauth/fiberauth/handshake.go:66` (`Register` — removed)
- Test: `oidcauth/fiberauth/gate_test.go` (add tests, update call sites), `oidcauth/fiberauth/bench_test.go` (update call sites)
- Modify: `README.md:144-150` (the login example)

**Interfaces:**
- Consumes: `declare.Component` from Task 1; `(*app.App).Declare`, `app.App.HTTP`, `app.App.Shutdown` from Task 1
- Produces: `fiberauth.New(a *app.App, o Options) *Gate`. `(*Gate).Register(*fiber.App)` is **removed** — `New` performs it.

`fiberauth` takes `*app.App` rather than `declare.Declarer` because it needs `a.HTTP` to mount routes, and it already imports Fiber so the dependency costs nothing. There are 13 `New(`/`Register(` call sites across two test files.

- [ ] **Step 1: Write the failing tests**

Append to `oidcauth/fiberauth/gate_test.go`:

```go
// newTestApp builds a real app for the gate to register against. fiberauth needs
// a.HTTP, so a fake declarer is not enough here.
func newTestApp(t *testing.T) *app.App {
	t.Helper()
	a, err := app.New(app.Options{
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

func componentNamed(a *app.App, name string) (app.Component, bool) {
	for _, c := range a.Components() {
		if c.Name == name {
			return c, true
		}
	}
	return app.Component{}, false
}

// One call builds the gate and puts it in the boot report, so a caller never
// writes Declare for login.
func TestNewDeclaresTheLoginComponent(t *testing.T) {
	a := newTestApp(t)
	New(a, Options{Sessions: newMemStore(), CookiePrefix: "testapp"})

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

// The handshake routes are mounted by New, not by a separate Register call that
// a caller can forget.
func TestNewMountsTheHandshakeRoutes(t *testing.T) {
	a := newTestApp(t)
	g := New(a, Options{Sessions: newMemStore(), CookiePrefix: "testapp"})

	resp, err := a.HTTP.Test(httptest.NewRequest(http.MethodGet, g.LoginPath(), nil),
		fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Test %s: %v", g.LoginPath(), err)
	}
	defer resp.Body.Close()

	// 404 would mean the route was never mounted. A disabled gate answers its own
	// status instead, which is what proves New registered it.
	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("%s returned 404 — New did not mount the handshake routes", g.LoginPath())
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
```

`newMemStore()`, `testProvider(t, baseURL)` and `newGate(t, p, store)` already exist in `gate_test.go`; `testProvider` builds a provider with `Issuer: "https://id.example.com"`, which is what the last assertion expects. `newGate` gains an `*app.App` parameter in Step 5. Ensure the file imports `io`, `log/slog`, `net/http`, `net/http/httptest`, `testing`, `time`, `github.com/gofiber/fiber/v3`, `github.com/neodata-io/neokit/app` and `github.com/neodata-io/neokit/config`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./oidcauth/fiberauth/ -run 'TestNewDeclares|TestNewMounts|TestADisabledGate' 2>&1 | head -5
```

Expected: FAIL to compile — `not enough arguments in call to New`.

- [ ] **Step 3: Change `New` to take the app and do the wiring**

In `oidcauth/fiberauth/gate.go`, change `New`'s signature and append the wiring before it returns. Replace `func New(o Options) *Gate {` with:

```go
// New builds the gate, mounts its handshake routes on a.HTTP and declares it in
// the boot report. One call — there is no separate Register step to forget.
//
// It cannot fail: a gate with no Provider is a working gate that is switched off.
//
// The session sweep stays yours: call [Gate.SweepJob] and start it on a context
// you own.
func New(a *app.App, o Options) *Gate {
```

At the end of the function, replace `return g` (or whatever the existing final return is) with:

```go
	g.register(a.HTTP)

	detail := "not configured"
	on := g.Enabled()
	if on {
		detail = g.Provider().Issuer()
	}
	a.Declare(app.Component{Name: "login", On: on, Detail: detail})
	return g
}
```

Add the import `"github.com/neodata-io/neokit/app"`. `oidcauth.Provider.Issuer() string` exists (`oidcauth/provider.go:60`), so the detail needs no fallback.

**The sweep is deliberately not started here.** `jobs.Job` has `Run` and `Start` but no `Stop` — a job is started with a context and ends when that context is cancelled. Starting a background goroutine inside a constructor would be hidden behaviour that a caller cannot opt out of or observe, which is the one risk the spec flags about this design. `SweepJob()` keeps its current shape and its current caller-started usage.

- [ ] **Step 4: Make `Register` unexported**

In `oidcauth/fiberauth/handshake.go:66`, rename the method so callers cannot mount the routes twice:

```go
func (g *Gate) register(app *fiber.App) {
```

Update the call in `New` to `g.register(a.HTTP)`.

- [ ] **Step 5: Update the existing call sites**

```bash
grep -rn 'New(Options\|= New(\|\.Register(' oidcauth/fiberauth/*_test.go
```

Most call sites go through the existing helper, so change it first:

```go
// newGate wires a gate onto a. A nil provider means "login not configured".
func newGate(t *testing.T, a *app.App, p *oidcauth.Provider, store oidcauth.SessionStore) *Gate {
	t.Helper()
	return New(a, Options{
		Provider:     func() *oidcauth.Provider { return p },
		Sessions:     store,
		CookiePrefix: "myapp",
		RateLimit:    -1, // off: these tests fire many requests from one peer
	})
}
```

Then each `newGate(t, p, store)` becomes `newGate(t, newTestApp(t), p, store)`, or takes an `a` the test already built when it needs to inspect it. Each direct `New(Options{...})` becomes `New(newTestApp(t), Options{...})`. Each `g.Register(someFiberApp)` is deleted — `New` does it now — and the test uses `a.HTTP` where it used its own `*fiber.App`. In `bench_test.go`, `t` is a `*testing.B`, so add a `newBenchApp(b *testing.B) *app.App` alongside `newTestApp` with the same body and `b.Fatalf`/`b.Cleanup`.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go test ./oidcauth/fiberauth/ -v 2>&1 | grep -E '^(--- (PASS|FAIL)|ok|FAIL)'
```

Expected: PASS for every test, including the three new ones.

- [ ] **Step 7: Update the README login example**

`README.md` lines 144-150 currently read `gate := fiberauth.New(fiberauth.Options{` … `gate.Register(app)`. Replace the whole example block with:

````markdown
```go
authn, ok := oidcauth.New(oidcauth.Config{
    Issuer:   os.Getenv("OIDC_ISSUER"),   ClientID:     os.Getenv("OIDC_CLIENT_ID"),
    BaseURL:  os.Getenv("OIDC_BASE_URL"), ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
})
gate := fiberauth.New(a, fiberauth.Options{
    Provider:     func() *oidcauth.Provider { if !ok { return nil }; return authn },
    Sessions:     store,      // your own storage; neokit ships none
    CookiePrefix: "myapp",
})
a.HTTP.Use(gate.ResolveIdentity())
admin := a.HTTP.Group("/api/v1/admin", gate.RequireOwner())
```

`fiberauth.New` mounts the handshake routes and adds the `login` line to the boot
report. `ok == false` (no credentials configured) means the gate is off: the
middleware returns immediately, the guards pass through, and the handshake routes
404 — so an app can ship open and close later without a second feature flag.

The session sweep stays yours — `if job, ok := gate.SweepJob(); ok { job.Start(a.Context()) }`.
````

Delete the paragraph that followed the old example, since its text is now folded into the block above.

- [ ] **Step 8: Run the full suite**

```bash
go build ./... && go vet ./... && go test ./... 2>&1 | grep -vE '^\?|^ok' || echo "FULL SUITE GREEN"
```

Expected: `FULL SUITE GREEN`.

- [ ] **Step 9: Commit**

```bash
git add oidcauth/fiberauth/ README.md
git commit -m "feat(fiberauth)!: New mounts, declares and schedules the gate

New takes the app, so one call builds the gate, mounts its handshake routes and
adds the login line to the boot report. Register is now unexported - there is no
separate step to forget or to run twice.

The session sweep is deliberately not started here: jobs.Job has no Stop, so it
would be a background goroutine a caller could neither observe nor opt out of.
SweepJob keeps its current caller-started shape.

fiberauth takes *app.App rather than declare.Declarer because it needs a.HTTP,
and it already imports Fiber so the dependency costs it nothing."
```

---

## Done when

- `go build ./... && go vet ./... && go test ./...` is clean
- `declare` imports only `context` — check with `go list -deps ./declare | grep -c neodata-io` returning `1` (itself)
- `app.Component` is an alias for `declare.Component`, and `var _ declare.Declarer = (*App)(nil)` compiles
- `sqlitex.Open(a, "database", path, migrate)` registers report line, readiness check and teardown
- `fiberauth.New(a, opts)` registers the `login` component and mounts the handshake routes; `SweepJob` is unchanged and still caller-started
- No `Subsystem` remains: `grep -rn 'Subsystem' --include='*.go' . | grep -v '^./docs'` returns nothing

## Not in scope

- `backup` and `notify` keep their current signatures.
- NeoGate's migration — separate work, tracked in the spec's Migration section: `sqlitex` (5 files), `oidcauth` (11 files), plus `server/cmd/api/main.go`.
- Authorization, shared cache and broker packages — separate specs.
