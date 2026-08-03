# Background work is a component

Date: 2026-08-03
Status: approved, ready to plan
Builds on: `2026-08-03-neokit-package-and-named-registration-design.md`

## Problem

`jobs` is the only first-class neokit concept with no place in the boot report,
no position in the shutdown order, and no feature that registers it. Three
consequences, all shipped today.

**The boot report can lie.** `backup.New` declares `✓ backups · daily, keep 7`
(`backup/backup.go:87`) and starts nothing. `Service.WriteDaily` is a method the
consumer must schedule. Forget it and the report still says backups are on, and
there are no backups. That is the drift `Component` exists to prevent — the same
argument `app/component.go` makes for readiness: *"a check that can fail readiness
while appearing nowhere in the report is invisible."*

**A documented shutdown contract that nothing implements.** `jobs.Job.Start`
(`jobs/jobs.go:90`) states its goroutine "is joined by `safe.WaitGo` at shutdown,
so a process that drains its background work before closing the resources these
jobs use gets that ordering for free." Nothing in `app` imports `safe`, and
`WaitGo` has no caller outside its own test. A job halfway through a write when
SIGTERM arrives has its database closed underneath it.

**neokit's own feature hands back homework.** `fiberauth` builds the expired-
session sweep and gives it to the caller — `oidcauth/fiberauth/gate.go:170`,
*"The session sweep stays yours"*. Every other feature registers itself; this one
is the exception, and an exception nobody notices until sessions accumulate.

## Decisions taken

- **`Component` gains `Run`.** A component already declares what it reports, how
  it is probed and how it is closed. How it is *started* is the missing fourth.
- **`app` starts and joins background work**, in a teardown position that puts
  the join before the caller's own `Close` steps.
- **`backup` and `fiberauth` start their own jobs.**
- **No new method on `App`.** The fixes are internal; a consumer's existing
  `job.Start(a.Context())` gains the shutdown wait without changing.

### Rejected, with reasons

**`a.RunsOnSchedule(job)` / `a.Every(job)`.** A public method for the consumer's
own jobs would buy exactly one thing: a line in the boot report. Not enough for a
method, its docs, its tests, and a second one for `jobs.Daily`. Reconsider if
services turn out to want their own jobs in the report.

**`app` holding its own `safe.Group` instead of draining the package-level one.**
`safe`'s own doc reserves the default group for "a binary's own composition root".
A private group would drain neokit's jobs correctly and leave every consumer job
started with `jobs.Job.Start` undrained — which is the bug. `app.Run` installs
signal handlers, owns the teardown order and blocks until the process exits; it
*is* the composition root a binary delegates to. `safe`'s comment is amended to
say so.

**Widening `declare.Declarer` so `backup` could call `job.Start` itself.** The
constructors receive a `Declarer`, which has no context and should not grow one.
Carrying the work on the `Component` keeps registration the single seam and keeps
`declare` importing only `context`.

**Type-detecting a `Run()` method on whatever is declared.** Same silent-failure
trade rejected in the previous spec: a mistyped signature registers nothing and
says nothing.

## Design

### 1. `declare`

```go
type Component struct {
	Name   string
	On     bool
	Detail string
	Ready  func(ctx context.Context) error
	Close  func(ctx context.Context) error

	// Run is the component's background work: started by app.Run on the
	// application context, joined during shutdown before any Close.
	Run func(ctx context.Context)
}

// Run registers fn as the component's background work.
func Run(fn func(ctx context.Context)) Option
```

`declare` still imports only `context`. `Run` is ignored when `On` is false, for
the reason `Ready` is: an unconfigured optional feature must not start doing work.
Unlike `Close`, there is nothing allocated to release.

### 2. `app` starts them

`App.Run` starts each declared component's work after the boot report prints and
before the listener goroutine:

```go
func (a *App) startBackgroundWork() {
	for _, c := range a.components {
		if !c.On || c.Run == nil {
			continue
		}
		safe.Go(a.ctx, c.Name, func() { c.Run(a.ctx) })
	}
}
```

After the report because `New` starts nothing and the report is the first thing a
process says; a job logging above it would be confusing. On `a.ctx` because that
context is cancelled by the `background-context` teardown step — after the HTTP
drain — so a job may keep running while requests finish and stops immediately
after.

`safe.Go` rather than a private group: it is the pool `jobs.Job.Start` already
uses, so one join covers neokit's jobs and the consumer's alike.

### 3. `app` joins them

One new step in `pushRunSteps`. The unwind order becomes:

```text
streams → api → background-context → background-work → [the caller's steps, reversed]
```

pushed in the reverse of that, so `background-work` is pushed first:

```go
// Before the caller's teardown, so a job is finished before the database it
// writes to closes. That position is the point of the step.
a.Shutdown.Push("background-work", func(ctx context.Context) error {
	return safe.WaitGo(budget(ctx))
})
```

`budget` is the step context's remaining deadline — `lifecycle.Stack.Shutdown`
bounds every step at `shutdownStepTimeout` (15s), and the drain should use that
rather than a second constant that can disagree with it.

`safe.WaitGo` changes from returning nothing to returning `error`, so a drain that
times out fails the step and the process exits non-zero. The change is source-
compatible: every call is a statement, and Go discards an unused return value
there.

Nothing is pushed when `App.Run` is never reached. On the early-return path
(`New` succeeds, the caller returns an error, `defer a.Close()` fires) no work was
started, so there is nothing to join.

### 4. `backup`

`Options` gains the schedule, as a named type so the report can render it and a
caller can read it back:

```go
// Clock is a local wall-clock time of day.
type Clock struct{ Hour, Minute int }

const DefaultHour = 3

// At is the local time the scheduled backup runs. Zero means DefaultHour:00.
At Clock
```

`New` builds a `jobs.Daily` and declares it:

```go
declare.Add(d, "backups",
	declare.Detail(fmt.Sprintf("daily at %s, keep %d", o.At, o.Retention)),
	declare.Run(svc.schedule(o.At).Run))
```

The job sets `RunAtStart`. `jobs.Daily` warns against it for announcements but
names this exact case as the exception — work "idempotent for the day (writing a
dated file)" — and `WriteDaily` is already pinned as a same-day no-op. Without it
a service that restarts every morning after its backup hour would never back up
at all.

Off with no `Dir`, as today, and then no job is declared. `backup` gains an import
of `jobs`.

### 5. `fiberauth`

The sweep joins the existing `login` declaration rather than adding a line of its
own — one feature, one name, which is the rule the previous spec set:

```go
opts := []declare.Option{declare.Detail(issuer)}
if job, ok := oidcauth.SweepJob(o.Sessions, log); ok {
	opts = append(opts, declare.Run(job.Run))
}
declare.Add(a, "login", opts...)
```

A gate declared off therefore does not sweep. That is correct rather than a gap:
with no provider configured nothing creates sessions, and a store holding only
expired ones is not a running service's problem.

`Gate.SweepJob` is removed. Leaving it exported alongside a gate that sweeps
itself means anyone following the current README starts a second sweep over the
same store. `oidcauth.SweepJob` stays — it is the building block for a service not
using `fiberauth`.

### 6. `safe`

`WaitGo` returns `error`, and its doc comment names `app.Run` as the sanctioned
caller of the process-wide default group.

### 7. Docs

- README: the "session sweep stays yours" paragraph goes.
- `oidcauth/fiberauth/gate.go:170`: same line, replaced by a statement that `New`
  starts the sweep.
- `jobs.Job.Start`: its shutdown claim becomes true and gains a pointer to `app`.

## Behaviour changes

`a.Backups(...)` starts writing a backup file every night at 03:00 and pruning to
the retention. `a.Login(...)` starts deleting expired sessions. Both are the point
of the change, and both are new behaviour for code that already calls them.

## Non-goals

- No method on `App` for the consumer's own jobs.
- No change to `jobs.Job` or `jobs.Daily` themselves.
- No change to the shutdown timeout constants.
- NeoGate is not migrated here.

## Testing

**The bug, first.** A component whose `Run` writes to a database registered
before it: on `Close`, the write completes before the database's `Close` runs.
This test fails today.

**`declare`**: `Run` sets the field; a component declared off carries it but
`app` does not start it.

**`app`**: a declared `Run` starts once on `App.Run` and receives a context
cancelled during teardown; the `background-work` step appears in the unwind order
between `background-context` and the caller's own steps; a job that never returns
fails the step rather than hanging past the budget; `Run` is never called on the
`New` → `Close` path.

**`backup`**: with a `Dir`, one component with a non-nil `Run` and a detail naming
the time and retention; without a `Dir`, one component, off, no `Run`. The default
schedule is 03:00; `Options.At` overrides it.

**`fiberauth`**: a store that can sweep gives the `login` component a `Run`; one
that cannot leaves it nil, and the component is still declared exactly once
either way.

**`safe`**: `WaitGo` reports a timeout rather than dropping it.

## Migration

**neokit:** `declare` (one field, one option), `app` (start + one teardown step),
`backup` (one option field, one job), `fiberauth` (one declaration, one method
removed), `safe` (one signature), README and `gate.go` docs.

**NeoGate:** any call to `gate.SweepJob()` is deleted rather than replaced. Any
hand-rolled nightly backup scheduling around `Service.WriteDaily` is deleted, or
kept and `backup.Options.Dir` left unset — running both would write the same file
twice.

## Risks

**Two backup schedules during migration.** A service already scheduling
`WriteDaily` gets a second one from `backup.New`. Named in the migration notes;
the file name is dated, so the second write is idempotent rather than duplicative,
but the pruning would run twice.

**The default group is process-wide.** A test that leaves a supervised goroutine
running is drained by the next `WaitGo` in the same binary. Pre-existing, and the
reason `safe` warns about it, but `app` calling `WaitGo` makes it reachable from
any test that runs an app.

## Style note

New doc comments are 1-3 lines, not the paragraph style of the surrounding code.
