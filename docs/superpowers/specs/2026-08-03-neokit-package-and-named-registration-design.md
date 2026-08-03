# The neokit package, and registration named for what it does

Date: 2026-08-03
Status: approved, ready to plan
Supersedes parts of: `2026-08-03-component-and-self-declaring-constructors-design.md`

## Amendment: registration is functional options

Chosen after reviewing every Go shape for the seam (struct literal, positional
args, type-detection, named helpers, options). Packages register through
`declare.Add(d, name, opts...)`:

```go
declare.Add(d, "database",
	declare.Detail(path),
	declare.Ready(db.PingContext),
	declare.Close(closeDB))

declare.Add(d, "backups", declare.Detail("daily, keep 7"))
declare.Add(d, "login", declare.Disabled("not configured"))
```

`Add` means on; `Disabled(why)` means off with the reason required. No struct
literals outside `declare`, no `On: true`, no nil placeholders — an absent
capability is an absent option. The `Component` struct remains exported (the
`Declarer` interface carries it) but nothing else constructs one by hand. The
App conveniences below become one-line wrappers over `Add`.

## Problem

`sqlitex.Open` and `fiberauth.New` now register themselves. Three things are still
wrong for a consumer.

**`backup` and `notify` do not.** So a developer has to remember which neokit
packages register themselves and which need a hand-written declaration. That
inconsistency costs more than the parameters it saves.

**`Declare` says nothing about what it does.** `a.Declare(app.Component{Name: …,
On: true, Detail: …, Close: …})` is a form to fill in, led by a verb that does not
describe the outcome. What it actually buys is shutdown ordering and readiness
checks — neither of which the name or the fields make obvious.

**The spelling is `sqlitex.Open(a, …)`, not `a.Database(…)`.** `app` cannot host
`a.Database` directly: importing `sqlitex`, `fiberauth`, `backup` and `notify`
takes `app` from 510 to 564 packages and forces `modernc.org/sqlite`, `libc`,
`go-oidc`, `go-jose` and `oauth2` into every service, including ones that use
none of them.

## Decisions taken

- **Two methods named for their effect**, replacing hand-written `Declare` calls:
  `ClosesOnShutdown` and `ChecksReadiness`.
- **`backup` and `notify` register themselves**, like `sqlitex` and `fiberauth`.
- **`sqlitex.Open` loses its name argument.** `OpenNamed` covers two databases;
  `ComponentName` is exported so tests never spell `"database"`.
- **A new root package `neokit`** wrapping `*app.App` by embedding, providing
  `Database`, `Login`, `Backups` and `Notify`. `app` imports nothing new.
- **No report-only registration.** A component must earn its line by being checked
  or closed, or by being one of neokit's own. NeoGate's `web push` line is
  dropped as a consequence, accepted deliberately.

### Rejected, with reasons

**`Feature(name, detail)` / `FeatureOff(name, why)`.** Once `sqlitex`,
`fiberauth`, `backup` and `notify` register themselves, `Feature` would be used
roughly once per service — for a line that only prints text. Not worth a public
method, its docs, its tests and a second name (`FeatureOff`/`Disabled`/`Off`)
whose only job was to pair with it.

**Type-detected registration** (`a.Use("plugins", manager)`, asserting for `Ping`
and `Close`). NeoGate's manager has `Close(ctx)` with no error return, so the
assertion would miss it and silently register nothing — the class of silent
failure removed from `Declare` in `f9003b0`. Trading a compile error for a log
line is the same trade rejected when reviewing neodata-go's `GetDB()`.

**Removing `Declare` from the API.** Not possible. `declare.Declarer` requires an
exported `Declare(Component)` method, and Go visibility is package-level, so
`*app.App` must export it for `sqlitex.Open(a, …)` to compile. It is instead
removed from the README, the examples and the package-doc usage block, and
documented as plumbing for packages that register themselves.

**Separating readiness registration from the report** (`a.HealthCheck(name, fn)`
with no report line). A check that can turn `/readyz` red while appearing nowhere
in the boot report is exactly what `app.go` already refuses: *"a check registered
around it would be invisible there."* Both new methods write a report line.

## Design

### 1. Two methods on `App`

```go
// ClosesOnShutdown registers close to run during teardown, in declaration order
// reversed, and adds name to the boot report.
func (a *App) ClosesOnShutdown(name, detail string, close func(ctx context.Context) error)

// ChecksReadiness registers ready as a /readyz check and adds name to the boot
// report.
func (a *App) ChecksReadiness(name, detail string, ready func(ctx context.Context) error)
```

Both are thin: they build a `Component` with `On: true` and call `Declare`. A nil
function is a caller error and panics, for the reason an empty `Name` does — a
registration that silently does nothing is worse than a crash at boot.

`Declare` and `Component` stay exported because the mechanism requires it, and
are documented as such.

### 2. `sqlitex`

```go
const ComponentName = "database"

func Open(d declare.Declarer, path string, migrate func(*sql.DB) error) (*sql.DB, error)
func OpenNamed(d declare.Declarer, name, path string, migrate func(*sql.DB) error) (*sql.DB, error)
```

`Open` declares `ComponentName`. `OpenNamed` is for a service with two databases.

### 3. `backup` and `notify`

```go
func backup.New(d declare.Declarer, s Snapshotter, o Options) *Service
func notify.NewWebhook(d declare.Declarer, url, secret string, o Options) *Webhook
func notify.NewNtfy(d declare.Declarer, topicURL, token string, o Options) *Ntfy
func notify.NewApprise(d declare.Declarer, url string, o Options) *Apprise
```

`backup` declares `"backups"`, on when `Options.Dir` is set, detail describing the
schedule and retention. Each `notify` sender declares under its existing
`Name()` — `"webhook"`, `"ntfy"`, `"apprise"` — so no name argument is needed and
two senders of different kinds cannot collide.

Neither has a `Ready` or a `Close`, so both are report-only components. That is
the one exception to "no report-only registration": the package supplies the
line, the consumer writes nothing.

### 4. The `neokit` root package

```go
package neokit // github.com/neodata-io/neokit

type App struct{ *app.App }

func New(o app.Options) (*App, error)

func (a *App) Database(path string, migrate func(*sql.DB) error) (*sql.DB, error)
func (a *App) Login(o fiberauth.Options) *fiberauth.Gate
func (a *App) Backups(s backup.Snapshotter, o backup.Options) *backup.Service

// One per sender, named for the service rather than a single Notify, because
// which backend you want is the decision being made at the call site.
func (a *App) Webhook(url, secret string, o notify.Options) *notify.Webhook
func (a *App) Ntfy(topicURL, token string, o notify.Options) *notify.Ntfy
func (a *App) Apprise(url string, o notify.Options) *notify.Apprise
```

Each method is three lines, forwarding to the package constructor with `a.App`.
Embedding means every existing `*app.App` method and field — `Run`, `Close`,
`HTTP`, `Shutdown`, `ClosesOnShutdown` — is available unchanged.

The methods **return** what they build. Nothing is stored for later retrieval:
there is no registry, no getter, and a missing dependency is a compile error.

### 5. The resulting `main.go`

```go
a, err := neokit.New(app.Options{Name: "neogate", Version: buildinfo.Get().Version, Base: cfg.Base})
if err != nil { return err }
defer a.Close()

db, err := a.Database(cfg.DatabasePath, migrate)
if err != nil { return err }

gate := a.Login(fiberauth.Options{Sessions: store, CookiePrefix: "neogate"})
a.Backups(store, backup.Options{Dir: backupDir, Retention: cfg.BackupRetention})

a.ClosesOnShutdown("plugins", fmt.Sprintf("%d loaded", len(plugins)), manager.Close)

return a.Run()
```

No `Declare`, no `Component`, no struct literal.

## Non-goals

- No `Feature`, `FeatureOff`, `Disabled` or `Off`.
- No type-detected registration.
- No service registry, no getters on `App`.
- `app` gains no imports. `cache`, `pubsub`, `webpush`, `ids`, `clock`, `safe`,
  `httpc`, `logx`, `netx` are unchanged — they are tools, not components.
- NeoGate is not migrated here.

## Testing

**`ClosesOnShutdown`**: the close runs on `App.Close`, in reverse declaration
order; the component appears in `Components()` with `On: true` and the given
detail; a nil close panics.

**`ChecksReadiness`**: the check reaches `/readyz` and turns it red when it
fails; the component appears in the report; a nil check panics.

**`sqlitex`**: `Open` declares `ComponentName`; `OpenNamed` declares the given
name; two `OpenNamed` calls with different names produce two components.

**`backup` / `notify`**: each constructor declares exactly one component with the
expected name, on-state and detail.

**`neokit`**: `New` returns an `App` whose embedded methods work; each of the four
methods registers exactly one component and returns the built value;
`var _ = (*neokit.App)(nil).Run` compiles, proving embedding exposes `app.App`.

## Migration

**neokit:** two methods on `App`; `sqlitex`, `backup` and `notify` signatures;
one new package; README and both examples rewritten to stop showing `Declare`.

**NeoGate:** not in scope. When it happens: `sqlitex` (5 files), `oidcauth`
(11 files), `backup` (1 file), `main.go`. Its `web push` declaration is deleted
rather than replaced.

## Risks

**`neokit` imports everything**, so anyone importing it compiles SQLite, OIDC and
the rest. That is the point — it is the batteries-included layer — but it must be
stated in its package doc, with a pointer to `app` for services that want less.

**Two ways to register** for a while: the new methods, and `Declare` underneath.
Mitigated by documentation, and by `Declare` disappearing from every example. If
nobody reaches for it in practice, it can be unexported later once no external
package needs it.

## Style note

New doc comments are 1-3 lines, not the paragraph style of the surrounding code.
