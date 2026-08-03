# neokit Root Package and Options Registration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Registration reads as `declare.Add(d, name, opts...)`; `backup` and `notify` register themselves; `sqlitex.Open` loses its name argument; a new root package `neokit` provides `a.Database`, `a.Login`, `a.Backups` and the notify senders; `Declare`/`Component` vanish from README and examples.

**Architecture:** `declare` gains `Add` plus four options (`Detail`, `Ready`, `Close`, `Disabled`); everything else funnels through it. `app` gains two consumer methods named for their effect. The root package wraps `*app.App` by embedding and forwards to the feature constructors — no registry, no getters, every method returns what it builds.

**Tech Stack:** Go 1.25.0, module `github.com/neodata-io/neokit`, stdlib `testing` only.

## Global Constraints

- **`declare` keeps importing only `context`.**
- **`app` gains no new imports** — the root package does the importing.
- **No report-only registration API for consumers** (`Feature` stays rejected); packages may declare their own report-only lines.
- **notify report details are host-only** — topic URLs are capability secrets (see the redaction test at `notify/notify_test.go:255`).
- **`Declare`/`Component` stay exported** (interface plumbing) but leave README, examples and doc-comment usage blocks.
- **Existing tests change only where a signature changed.**
- Spec: `docs/superpowers/specs/2026-08-03-neokit-package-and-named-registration-design.md`

---

### Task 1: `declare.Add` and options

**Files:** Modify `declare/declare.go`, `declare/declare_test.go`.

**Produces:** `type Option func(*Component)`; `Detail(string) Option`; `Ready(func(context.Context) error) Option`; `Close(func(context.Context) error) Option`; `Disabled(why string) Option`; `Add(d Declarer, name string, opts ...Option)`.

- [ ] Failing tests (append to `declare/declare_test.go`; `recorder` exists there):

```go
// Add means on: the common case carries no boolean.
func TestAddDefaultsToOn(t *testing.T) {
	var r recorder
	declare.Add(&r, "backups", declare.Detail("daily, keep 7"))
	c := r.got[0]
	if !c.On || c.Name != "backups" || c.Detail != "daily, keep 7" {
		t.Errorf("Component = %+v", c)
	}
	if c.Ready != nil || c.Close != nil {
		t.Error("options not passed must stay nil")
	}
}

// Disabled is the ✗ line, and the reason is the payload.
func TestDisabledTurnsOffWithReason(t *testing.T) {
	var r recorder
	declare.Add(&r, "login", declare.Disabled("not configured"))
	c := r.got[0]
	if c.On || c.Detail != "not configured" {
		t.Errorf("Component = %+v", c)
	}
}

// Ready and Close carry through, and errors survive the trip.
func TestReadyAndCloseCarryThrough(t *testing.T) {
	var r recorder
	boom := errors.New("boom")
	declare.Add(&r, "cache",
		declare.Ready(func(context.Context) error { return boom }),
		declare.Close(func(context.Context) error { return nil }))
	c := r.got[0]
	if err := c.Ready(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Ready err = %v", err)
	}
	if c.Close == nil || c.Close(context.Background()) != nil {
		t.Error("Close missing or failing")
	}
}
```

- [ ] Run: `go test ./declare/ -run 'TestAdd|TestDisabled|TestReadyAnd'` — expect `undefined: declare.Add`.
- [ ] Implement in `declare/declare.go`:

```go
// Option configures one aspect of a Component. Applied in order; last wins.
type Option func(*Component)

// Detail sets the report line's context: a path, an issuer, a schedule.
func Detail(s string) Option { return func(c *Component) { c.Detail = s } }

// Ready registers fn as the component's readiness check.
func Ready(fn func(ctx context.Context) error) Option {
	return func(c *Component) { c.Ready = fn }
}

// Close registers fn to run during shutdown.
func Close(fn func(ctx context.Context) error) Option {
	return func(c *Component) { c.Close = fn }
}

// Disabled marks the component off; why is the report's explanation.
func Disabled(why string) Option {
	return func(c *Component) { c.On = false; c.Detail = why }
}

// Add declares a component on d — on by default, shaped by opts. This is the
// registration seam every neokit package funnels through.
func Add(d Declarer, name string, opts ...Option) {
	c := Component{Name: name, On: true}
	for _, o := range opts {
		o(&c)
	}
	d.Declare(c)
}
```

- [ ] `go test ./declare/` green; commit `feat(declare): Add with functional options`.

### Task 2: App methods, and app's own lines via Add

**Files:** Modify `app/component.go` (methods + `otelComponent`/`metricsComponent`/`healthComponent` converted to `declare.Add` style), `app/app.go` (call sites), test `app/component_convenience_test.go` (new).

**Produces:** `(a *App) ClosesOnShutdown(name, detail string, close func(context.Context) error)`; `(a *App) ChecksReadiness(name, detail string, ready func(context.Context) error)`. Nil function panics.

- [ ] Failing tests (new file, `package app_test`, reuse `newApp`):

```go
func TestClosesOnShutdownClosesAndReports(t *testing.T) {
	a := newApp(t)
	closed := false
	a.ClosesOnShutdown("plugins", "3 loaded", func(context.Context) error { closed = true; return nil })
	if _, ok := findComponent(a, "plugins"); !ok {
		t.Fatal("plugins missing from the report")
	}
	if err := a.Close(); err != nil || !closed {
		t.Errorf("closed=%v err=%v", closed, err)
	}
}

func TestChecksReadinessReports(t *testing.T) {
	a := newApp(t)
	a.ChecksReadiness("cache", "redis://localhost", func(context.Context) error { return errors.New("down") })
	c, ok := findComponent(a, "cache")
	if !ok || !c.On || c.Detail != "redis://localhost" {
		t.Fatalf("component = %+v ok=%v", c, ok)
	}
}

func TestNilFunctionsPanic(t *testing.T) {
	a := newApp(t)
	for _, f := range []func(){
		func() { a.ClosesOnShutdown("x", "", nil) },
		func() { a.ChecksReadiness("x", "", nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("nil function must panic — silent no-op registration is the bug class this API removes")
				}
			}()
			f()
		}()
	}
}
```

`findComponent(a, name)` — add tiny helper to this file (loop over `a.Components()`).

- [ ] Implement (in `app/component.go`):

```go
// ClosesOnShutdown registers close to run during teardown — after the HTTP
// drain, in reverse declaration order — and adds name to the boot report. For a
// dependency you built; neokit's own packages register themselves.
func (a *App) ClosesOnShutdown(name, detail string, close func(ctx context.Context) error) {
	if close == nil {
		panic("app: ClosesOnShutdown requires a close function")
	}
	declare.Add(a, name, declare.Detail(detail), declare.Close(close))
}

// ChecksReadiness registers ready as a /readyz check and adds name to the boot
// report. The check and the report line travel together on purpose — a check
// that can fail readiness while appearing nowhere in the report is invisible.
func (a *App) ChecksReadiness(name, detail string, ready func(ctx context.Context) error) {
	if ready == nil {
		panic("app: ChecksReadiness requires a ready function")
	}
	declare.Add(a, name, declare.Detail(detail), declare.Ready(ready))
}
```

(`context` returns to `app/component.go`'s imports.)

- [ ] Convert the three internal report helpers so `app` itself uses `Add` — `otelComponent(name)` becomes `declareOTEL(a, name)` calling `declare.Add(a, name, declare.Detail(endpoint))` or `declare.Disabled("OTEL_EXPORTER_OTLP_ENDPOINT unset")`; same treatment for the metrics-endpoint and health lines. Update the four call sites in `app/app.go:216-228`. Behaviour identical — existing report tests are the referee.
- [ ] Full suite green; commit `feat(app): ClosesOnShutdown and ChecksReadiness`.

### Task 3: `sqlitex.Open` drops the name

**Files:** Modify `sqlitex/open.go`; update 12 test call sites (`open_test.go`, `query_test.go`).

**Produces:** `const ComponentName = "database"`; `Open(d declare.Declarer, path string, migrate func(*sql.DB) error)`; `OpenNamed(d declare.Declarer, name, path string, migrate func(*sql.DB) error)`.

- [ ] Rename existing `Open` → `OpenNamed` (same body; declaration switches to `declare.Add(d, name, declare.Detail(path), declare.Ready(db.PingContext), declare.Close(...))`). Add:

```go
// ComponentName is what Open declares. Tests and scrape configs use it instead
// of retyping "database".
const ComponentName = "database"

// Open opens the service's database and declares it as [ComponentName]. Use
// [OpenNamed] only when one service opens two.
func Open(d declare.Declarer, path string, migrate func(*sql.DB) error) (*sql.DB, error) {
	return OpenNamed(d, ComponentName, path, migrate)
}
```

- [ ] Test updates: existing `Open(&recorder{}, "database", …)` → `Open(&recorder{}, …)`; `TestTwoDatabasesDeclareDistinctNames` switches to `OpenNamed`; `TestOpenDeclaresTheDatabase` asserts `c.Name == sqlitex.ComponentName`.
- [ ] Full suite green (fix `examples/production-service` call — drops its `"database"` argument); commit `feat(sqlitex)!: Open declares ComponentName; OpenNamed for a second database`.

### Task 4: `backup` and `notify` register themselves

**Files:** Modify `backup/backup.go`, `notify/notify.go`; update call sites in `backup/backup_test.go` (9), `notify/notify_test.go` (~15).

**Produces:**

```go
func backup.New(d declare.Declarer, s Snapshotter, o Options) *Service
func notify.NewWebhook(d declare.Declarer, url, secret string, opts Options) *Webhook
func notify.NewNtfy(d declare.Declarer, topicURL, token string, opts Options) *Ntfy
func notify.NewApprise(d declare.Declarer, url string, opts Options) *Apprise
```

- [ ] `backup.New` declares after construction: `Dir` empty → `declare.Add(d, "backups", declare.Disabled("no backup directory configured"))`; otherwise `declare.Add(d, "backups", declare.Detail(fmt.Sprintf("daily, keep %d", retention)))`.
- [ ] Each notify constructor declares under its `Name()` with host-only detail:

```go
// reportDetail is the safe half of a notification URL: ntfy topics and hook
// paths are capability secrets, so only the host reaches the boot report.
func reportDetail(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
```

`declare.Add(d, w.Name(), declare.Detail(reportDetail(url)))` in each.

- [ ] Failing tests first (one per package): `TestNewDeclaresBackups` (on with dir, disabled without), `TestSendersDeclareHostOnly` (construct `NewNtfy(&r, srv.URL+"/secret-topic", …)`, assert detail has no path). Then mechanical call-site sweep: `sed` `New(snap,` → `New(&recorder{}, snap,` etc.; each test package gains the 3-line `recorder`.
- [ ] Full suite green; commit `feat(backup,notify)!: constructors declare themselves`.

### Task 5: the `neokit` root package

**Files:** Create `neokit.go`, `neokit_test.go` at repo root.

**Produces:**

```go
package neokit

type App struct{ *app.App }
func New(o app.Options) (*App, error)
func (a *App) Database(path string, migrate func(*sql.DB) error) (*sql.DB, error)
func (a *App) Login(o fiberauth.Options) *fiberauth.Gate
func (a *App) Backups(s backup.Snapshotter, o backup.Options) *backup.Service
func (a *App) Webhook(url, secret string, o notify.Options) *notify.Webhook
func (a *App) Ntfy(topicURL, token string, o notify.Options) *notify.Ntfy
func (a *App) Apprise(url string, o notify.Options) *notify.Apprise
```

Each method forwards to the package constructor with `a.App`. Package doc states the trade: importing `neokit` compiles every feature (SQLite, OIDC, …); import `neokit/app` and the packages you use for less.

- [ ] Failing tests: `TestDatabaseRegistersAndReturns` (open via `a.Database`, assert handle works and `Components()` contains `sqlitex.ComponentName`); `TestLoginRegistersAndMounts` (gate returned, `"login"` component present); `TestBackupsRegisters`; `TestEmbeddingExposesApp` (`a.HTTP`, `a.ClosesOnShutdown`, `a.Close` reachable — compile-time by use).
- [ ] Implement (~60 lines); full suite green; commit `feat: neokit root package — a.Database, a.Login, a.Backups, senders`.

### Task 6: README and examples stop showing Declare

**Files:** Modify `README.md`, `examples/production-service/main.go`, `app/app.go` package doc.

- [ ] `production-service` switches to `neokit.New` + `a.Database(...)` — the example demonstrates the top layer.
- [ ] README quick-start section gains the root-package form; the component section shows `a.ClosesOnShutdown` / `a.ChecksReadiness`; every `a.Declare(app.Component{…})` disappears; the layering table (app-only vs neokit) gets three lines.
- [ ] `app/app.go` package-doc example: replace the `a.Declare(app.Component{…})` block with `sqlitex.Open(a, cfg.DatabasePath, migrate)` and note `Declare` is the seam packages register through.
- [ ] `go build ./... && go vet ./... && go test ./...` strict (build/vet/test each checked separately — no `&&`-chain into `||`); commit `docs: the neokit layer; Declare leaves the examples`.

## Done when

- `declare` still stdlib-only (`go list -deps ./declare | grep -c neodata-io` → 1)
- `grep -rn 'Declare(app.Component' README.md examples/` → nothing
- Root import works: a test file in `neokit_test.go` uses `neokit.New(...).Database(...)`
- Full suite + `-race` on changed packages green

## Not in scope

- NeoGate migration (17 files; separate decision)
- `Feature`/`FeatureOff` (rejected); type-detected registration (rejected)
- `webpush` (report-only; consciously dropped from the report)
