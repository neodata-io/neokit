// Package jobs runs periodic background work correctly.
//
// The hand-written ticker loop it replaces is four lines, and each copy is a
// chance to omit one of the three things that make it safe:
//
//   - a **per-tick timeout**. A scheduler's ctx lives for the whole process, so
//     a call that never returns does not lose one sample — the tick never
//     completes, the loop never fires again, and the job is silently dead for
//     the rest of the process's life. No crash, no log line.
//   - **panic containment**. A panic on a bare `go` ends that goroutine forever,
//     or unrecovered takes the process with it.
//   - **a run at start**. A restart is when the work is most overdue, and a plain
//     ticker waits out a full interval first.
//
// [Job] is a plain struct: every knob is a field readable at the call site, and
// no scheduler object owns your goroutines.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/safe"
)

// Job is one piece of periodic work. The zero value is not runnable: Name,
// Every and Do are required.
type Job struct {
	// Name identifies the job in logs and in panic reports. Required.
	Name string

	// Every is the gap between runs. Required, and must be positive — see [Job.Run].
	Every time.Duration

	// Timeout bounds a single run. Zero means unbounded, which is almost always
	// wrong for anything touching a network: see the package doc. It is not
	// defaulted, because a silent default would be a policy this package has no
	// business setting — but an unbounded job that also has no cancellable Do is
	// the one failure mode with no external symptom.
	Timeout time.Duration

	// RunAtStart runs Do once immediately, before the first tick.
	RunAtStart bool

	// Do is the work. It must honour ctx: cancellation is how both shutdown and
	// Timeout reach it. Required.
	Do func(ctx context.Context) error

	// OnError handles a failed run. Nil logs at warn with the canonical error
	// key. A caller whose upstream is expected to be down sometimes uses this to
	// demote the noise — a job that ticks all day would otherwise emit an
	// identical warning on every tick for as long as the upstream is out.
	OnError func(ctx context.Context, err error)

	// Log receives the job's own diagnostics. Nil means slog.Default().
	Log *slog.Logger
}

// Run drives the job until ctx is cancelled, then returns. It blocks.
//
// It panics if Every is not positive, matching [time.NewTicker]'s contract:
// treating it as "run once" would turn a periodic job into a one-shot with
// nothing to notice, the silent failure this package exists to remove.
func (j Job) Run(ctx context.Context) {
	if j.Every <= 0 {
		panic("jobs: Job.Every must be positive (job " + j.Name + ")")
	}
	if j.Do == nil {
		panic("jobs: Job.Do must be set (job " + j.Name + ")")
	}

	if j.RunAtStart {
		j.tick(ctx)
	}

	ticker := time.NewTicker(j.Every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.tick(ctx)
		}
	}
}

// Start runs the job on a supervised background goroutine and returns
// immediately. app.Run joins that goroutine during teardown, before the
// application's own Close steps — so a job started this way finishes its write
// before the store it writes to closes.
//
// Use [Job.Run] directly when you own the goroutine — for instance to join it
// with a [safe.Group] of your own rather than the package-level default.
func (j Job) Start(ctx context.Context) {
	safe.Go(ctx, "job:"+j.Name, func() { j.Run(ctx) })
}

// tick runs Do once: bounded, guarded, and never able to end the loop.
//
// The panic guard is what keeps a single bad run from stopping the schedule. A
// panic inside Do would otherwise unwind through Run and out of the goroutine —
// under safe.Go that respawns the whole job after a backoff, which loses the
// RunAtStart/ticker phase; recovering here keeps the loop's own state and simply
// skips the one run.
func (j Job) tick(ctx context.Context) {
	if j.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, j.Timeout)
		defer cancel()
	}

	var err error
	if panicked := safe.Do("job:"+j.Name, func() { err = j.Do(ctx) }); panicked {
		return // already logged with a stack trace by safe.Do
	}
	if err == nil {
		return
	}
	if j.OnError != nil {
		j.OnError(ctx, err)
		return
	}
	j.logger().WarnContext(ctx, "background job failed", "job", j.Name, logx.Err(err))
}

func (j Job) logger() *slog.Logger {
	if j.Log != nil {
		return j.Log
	}
	return slog.Default()
}
