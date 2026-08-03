# Subsystem.Close Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one `app.Declare` call cover a dependency's boot report line, its readiness check and its teardown step, so its name is typed once instead of twice.

**Architecture:** Three lines of behaviour behind a nil check. `app.Subsystem` gains a `Close func(ctx) error` field that `Declare` pushes onto the existing `App.Shutdown` stack at declaration time, preserving LIFO unwinding. `lifecycle.Closer` adapts the common `Close() error` shape into a `lifecycle.Step`. Documentation states that neokit builds on Fiber v3 by choice.

**Tech Stack:** Go 1.25.0, module `github.com/neodata-io/neokit`. Standard library `testing` only — the `app` and `lifecycle` suites use no assertion library.

## Global Constraints

- **Additive only.** No existing test may be modified. If an existing test breaks, stop and report — that is the signal the change was not additive.
- **The public API stays Fiber-based.** No `web` package, no renaming of `fiberx` or `oidcauth/fiberauth`, no change to `App.HTTP`'s type. This was decided; see the spec's "Decisions taken, including what was rejected".
- **Do not add `Options.MapError` or `Options.QuietPath`.** They were designed and rejected.
- **New doc comments are 1-3 lines**, not the paragraph style of the surrounding code.
- **`Close` runs whenever it is non-nil, including when `On` is false** — unlike `Ready`, which is ignored when off.
- Spec: `docs/superpowers/specs/2026-08-03-subsystem-close-and-fiber-docs-design.md`

**Prerequisite — read before Task 1.** At the time of writing the working tree had staged *and* unstaged changes across `app/`, `health/`, `metrics/`, `config/`, `go.mod` and `README.md`. Land those first. The `git add` + `git commit` steps below assume a clean index; if the index still holds unrelated work, use the pathspec form instead, which commits only the named files:

```bash
git commit <paths...> -m "<message>"
```

Verify before starting:

```bash
git status --short
```

Expected: empty, or only `docs/superpowers/` entries.

---

### Task 1: `lifecycle.Closer`

**Files:**
- Modify: `lifecycle/lifecycle.go` (insert after `PushCloser`, which ends at line 100)
- Test: `lifecycle/lifecycle_test.go` (append)

**Interfaces:**
- Consumes: `lifecycle.Step` — `type Step func(ctx context.Context) error`, already defined at `lifecycle/lifecycle.go:53`
- Produces: `lifecycle.Closer(c interface{ Close() error }) lifecycle.Step` — returns nil when `c` is nil

`lifecycle_test.go` is an **internal** test (`package lifecycle`) and already defines a `quiet()` helper at its top. Do not redefine it.

- [ ] **Step 1: Write the failing tests**

Append to `lifecycle/lifecycle_test.go`:

```go
// closerFunc is the Close() error shape most libraries expose.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// Closer is the bridge between that shape and a Step, which takes a context.
// The error has to survive the trip, or a failed close is silently a success.
func TestCloserAdaptsAnIoCloser(t *testing.T) {
	want := errors.New("boom")
	step := Closer(closerFunc(func() error { return want }))
	if step == nil {
		t.Fatal("Closer returned nil for a non-nil closer")
	}
	if err := step(context.Background()); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// A nil closer yields a nil Step, which Push already ignores — the same
// tolerance for a wiring mistake that PushCloser has.
func TestCloserOfNilIsNil(t *testing.T) {
	if Closer(nil) != nil {
		t.Error("Closer(nil) must be nil so Push ignores it")
	}
}
```

`errors` and `context` are already imported by this file; no import changes needed.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./lifecycle/ -run 'TestCloser' -v
```

Expected: FAIL to compile, with `undefined: Closer`.

- [ ] **Step 3: Write the implementation**

In `lifecycle/lifecycle.go`, insert immediately after `PushCloser` (after the closing brace on line 100) and before `// Len reports how many steps are registered.`:

```go
// Closer adapts an ordinary Close() error into a [Step], for a caller that holds
// the step value rather than pushing it straight onto a [Stack]. Nil in, nil
// out, which [Stack.Push] already ignores.
func Closer(c interface{ Close() error }) Step {
	if c == nil {
		return nil
	}
	return func(context.Context) error { return c.Close() }
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./lifecycle/ -run 'TestCloser' -v
```

Expected: PASS for both `TestCloserAdaptsAnIoCloser` and `TestCloserOfNilIsNil`.

- [ ] **Step 5: Run the full lifecycle suite**

```bash
go test ./lifecycle/
```

Expected: `ok`. No existing test changed behaviour.

- [ ] **Step 6: Commit**

```bash
git add lifecycle/lifecycle.go lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): add Closer, adapting an io.Closer to a Step"
```

---

### Task 2: `Subsystem.Close`

**Files:**
- Modify: `app/subsystem.go:10-33` (the `Subsystem` type and its doc), `app/subsystem.go:35-46` (`Declare` and its doc)
- Create: `app/subsystem_close_test.go`

**Interfaces:**
- Consumes: `lifecycle.Closer` from Task 1; `App.Shutdown` (`*lifecycle.Stack`, exported field on `App`); `Stack.Push(name string, fn Step)` and `Stack.Len() int`
- Produces: `app.Subsystem.Close func(ctx context.Context) error`, pushed by `Declare` when non-nil

`app/app_test.go` is an **external** test (`package app_test`) and already defines `quiet()` and `newApp(t *testing.T) *app.App`. The new file is in the same package and uses `newApp`; do not redefine either helper.

`newApp` registers `t.Cleanup(func() { _ = a.Close() })`. Calling `a.Close()` explicitly inside a test is correct — `Close` is idempotent, so the cleanup's second call is inert.

- [ ] **Step 1: Write the failing tests**

Create `app/subsystem_close_test.go`:

```go
package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/lifecycle"
)

// fakeStore stands in for a dependency with the Close() error shape.
type fakeStore struct{ closed bool }

func (s *fakeStore) Close() error { s.closed = true; return nil }

// The teardown half of a declaration has to actually run, or the single call
// that replaced PushCloser + Declare has quietly dropped the teardown.
func TestSubsystemCloseRunsOnShutdown(t *testing.T) {
	a := newApp(t)
	store := &fakeStore{}

	a.Declare(app.Subsystem{
		Name: "database", On: true, Detail: "/tmp/test.db",
		Close: lifecycle.Closer(store),
	})

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !store.closed {
		t.Error("the declared Close never ran")
	}
}

// Close runs even when the subsystem is off, unlike Ready. A non-nil Close means
// something was allocated, and skipping it because a feature reports off leaks it.
func TestSubsystemCloseRunsWhenTheSubsystemIsOff(t *testing.T) {
	a := newApp(t)

	closed := false
	a.Declare(app.Subsystem{
		Name: "backups", On: false, Detail: "disabled",
		Close: func(context.Context) error { closed = true; return nil },
	})

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Error("Close was skipped because On was false; it must still run")
	}
}

// Declaration order is push order and the stack unwinds in reverse, so the
// dependency declared last is released first — the property Push exists for.
func TestSubsystemCloseUnwindsInReverseDeclarationOrder(t *testing.T) {
	a := newApp(t)

	var order []string
	for _, name := range []string{"database", "cache", "bus"} {
		a.Declare(app.Subsystem{
			Name: name, On: true,
			Close: func(context.Context) error {
				order = append(order, name)
				return nil
			},
		})
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []string{"bus", "cache", "database"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// A report-only subsystem must register no step: Declare still has to serve the
// things that hold nothing, which is most of neokit's own declarations.
func TestSubsystemWithoutCloseDoesNotTouchTheStack(t *testing.T) {
	a := newApp(t)

	// New has already pushed its own steps (tracing, metrics-export), so this
	// compares against that baseline rather than an absolute count.
	before := a.Shutdown.Len()
	a.Declare(app.Subsystem{Name: "report-only", On: true, Detail: "nothing to close"})

	if got := a.Shutdown.Len(); got != before {
		t.Errorf("stack grew from %d to %d; a nil Close must push nothing", before, got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./app/ -run 'TestSubsystem' -v
```

Expected: FAIL to compile, with `unknown field Close in struct literal of type app.Subsystem`.

- [ ] **Step 3: Add the `Close` field**

In `app/subsystem.go`, replace the `Subsystem` doc comment and add the field after `Ready`. The type doc changes because the declaration now produces three outputs, not two:

```go
// Subsystem is one optional part of the process: whether it is on, a line for
// the boot report, a readiness check when it has a dependency worth probing, and
// a teardown step when it holds something.
//
// One declaration produces all of them, so a subsystem is named once and cannot
// appear in the report under one name and in /readyz or the shutdown log under
// another.
```

Then, immediately after the `Ready` field's closing line, add:

```go
	// Close releases what this subsystem holds; [App.Declare] pushes it onto
	// [App.Shutdown]. Unlike Ready it runs even when On is false — a non-nil
	// Close means something was allocated and would otherwise leak.
	Close func(ctx context.Context) error
```

- [ ] **Step 4: Push it from `Declare`**

In `app/subsystem.go`, update `Declare`'s doc comment and body:

```go
// Declare records a subsystem for the boot report, for readiness when it is on
// and has a check, and for teardown when it has a Close.
//
// Call it during boot, before [App.Run], from one goroutine: the report renders
// at the top of Run, so a later declaration would miss it anyway.
func (a *App) Declare(s Subsystem) {
	a.subsystems = append(a.subsystems, s)

	if s.On && s.Ready != nil {
		a.health.Register(s.Name, s.Ready)
	}
	// Pushed here rather than at Run, so it unwinds among the caller's own steps
	// in the order they were declared.
	if s.Close != nil {
		a.Shutdown.Push(s.Name, s.Close)
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test ./app/ -run 'TestSubsystem' -v
```

Expected: PASS for all four tests.

- [ ] **Step 6: Run the full suite**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: `ok` for every package. **No existing test may have changed behaviour** — if one fails, stop and report rather than editing it.

- [ ] **Step 7: Commit**

```bash
git add app/subsystem.go app/subsystem_close_test.go
git commit -m "feat(app): declare a subsystem's teardown alongside its report line"
```

---

### Task 3: State the Fiber choice

**Files:**
- Modify: `app/app.go:1-7` (package doc)
- Modify: `README.md:6` and `README.md:119-120`

**Interfaces:**
- Consumes: `app.Subsystem.Close` from Task 2, `lifecycle.Closer` from Task 1 — both appear in the README example
- Produces: nothing code-facing

No tests: this task changes documentation only. Its verification is that the package still builds and the rendered doc reads correctly.

- [ ] **Step 1: State it in the `app` package doc**

In `app/app.go`, insert a paragraph between the opening paragraph and `// It is deliberately *not* a container.` — i.e. after line 2's text and its following `//` separator:

```go
// neokit builds on Fiber v3. App.HTTP is a *fiber.App and handlers are ordinary
// Fiber handlers — a deliberate choice, not an implementation detail waiting to
// be abstracted away.
//
```

- [ ] **Step 2: State it in the README intro**

In `README.md`, after the opening paragraph that ends `observability, caching, and optional integrations.` (line 6), insert a blank line and:

```markdown
neokit builds on [Fiber v3](https://github.com/gofiber/fiber). `app.HTTP` is a
`*fiber.App` and handlers are ordinary Fiber handlers — a deliberate choice, not
an implementation detail waiting to be abstracted away.
```

- [ ] **Step 3: Document `Close` in the README's subsystem section**

In `README.md`, after the paragraph ending `so it cannot drift from what the process actually is.` (line 120), insert a blank line and:

````markdown
Give a declaration a `Close` and it is the teardown step too, so a dependency is
named once rather than once per concern:

```go
a.Declare(app.Subsystem{
    Name: "database", On: true, Detail: cfg.DatabasePath,
    Ready: store.Ping,
    Close: lifecycle.Closer(store),
})
```
````

- [ ] **Step 4: Verify the build and read the rendered doc**

```bash
go build ./... && go doc github.com/neodata-io/neokit/app | head -30
```

Expected: builds cleanly, and the Fiber paragraph appears in the package doc output.

- [ ] **Step 5: Run the full suite once more**

```bash
go test ./...
```

Expected: `ok` for every package.

- [ ] **Step 6: Commit**

```bash
git add app/app.go README.md
git commit -m "docs: state that neokit builds on Fiber v3, and document Subsystem.Close"
```

---

## Done when

- `go build ./... && go vet ./... && go test ./...` is clean
- `app.Subsystem` has a `Close` field that `Declare` pushes when non-nil
- `lifecycle.Closer` exists and returns nil for a nil closer
- The `app` package doc and the README both state the Fiber choice
- No pre-existing test was modified

## Not in scope

- Migrating NeoGate. Adoption is opt-in; its `PushCloser` + `Declare` pair keeps working unchanged.
- `Shutdown.Push` and `Shutdown.PushCloser` are untouched and stay the right tool for steps that are not subsystems.
