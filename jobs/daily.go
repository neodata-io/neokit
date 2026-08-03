package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/safe"
)

// Daily runs work once a day at a local wall-clock time — a morning digest, an
// overnight report, a 3am compaction.
//
// It is separate from [Job] rather than a special interval because a day is not
// 24 hours. Twice a year it is 23 or 25, and a job expressed as
// `Every: 24 * time.Hour` walks an hour off its intended time at each DST
// transition and never walks back: an 08:00 digest becomes 07:00 in spring, then
// 06:00 the next spring. Daily recomputes the next occurrence from the calendar
// each time, so it always fires at the stated local hour.
//
// The zero value is not runnable: Name and Do are required.
type Daily struct {
	// Name identifies the job in logs and in panic reports. Required.
	Name string

	// Hour and Minute are the local time to fire at, in the location [Daily.Now]
	// reports. Hour must be 0–23 and Minute 0–59.
	Hour   int
	Minute int

	// Timeout bounds a single run. Zero means unbounded — see [Job.Timeout].
	Timeout time.Duration

	// RunAtStart runs Do once immediately, before waiting for the next occurrence.
	//
	// Usually wrong for a daily job and off by default: a process that restarts
	// five times in an afternoon would send five morning digests. Turn it on only
	// when the work is idempotent for the day (writing a dated file, refreshing a
	// cache) rather than an announcement.
	RunAtStart bool

	// Do is the work. It must honour ctx. Required.
	Do func(ctx context.Context) error

	// OnError handles a failed run. Nil logs at warn. See [Job.OnError].
	OnError func(ctx context.Context, err error)

	// Log receives the job's own diagnostics. Nil means slog.Default().
	Log *slog.Logger

	// Now is the clock. Nil means time.Now. The location it reports is the one the
	// Hour/Minute are interpreted in, which is what makes "08:00" mean 08:00 to the
	// household rather than 08:00 UTC.
	Now func() time.Time
}

// Run drives the job until ctx is cancelled, then returns. It blocks.
//
// It panics on an out-of-range Hour or Minute, or a nil Do: each is a
// programming error that would otherwise silently fire at the wrong time.
func (d Daily) Run(ctx context.Context) {
	if d.Hour < 0 || d.Hour > 23 {
		panic("jobs: Daily.Hour must be 0-23 (job " + d.Name + ")")
	}
	if d.Minute < 0 || d.Minute > 59 {
		panic("jobs: Daily.Minute must be 0-59 (job " + d.Name + ")")
	}
	if d.Do == nil {
		panic("jobs: Daily.Do must be set (job " + d.Name + ")")
	}

	if d.RunAtStart {
		d.tick(ctx)
	}

	for {
		// Recomputed every iteration rather than advanced by a fixed step: that is
		// the whole point (see the type doc), and it also means a job that overran
		// its slot lands on the next occurrence rather than immediately re-firing.
		//
		// The delay is derived from [Daily.Now] on both sides — not time.Until,
		// which measures against the real wall clock. Mixing the two makes an
		// injected clock produce a delay with no relation to the schedule it was
		// injected to control.
		wait := time.NewTimer(d.delay(d.now()))
		select {
		case <-ctx.Done():
			wait.Stop()
			return
		case <-wait.C:
			d.tick(ctx)
		}
	}
}

// Start runs the job on a supervised background goroutine and returns
// immediately. See [Job.Start].
func (d Daily) Start(ctx context.Context) {
	safe.Go(ctx, "job:"+d.Name, func() { d.Run(ctx) })
}

// Next returns the first occurrence strictly after from.
//
// It advances by a *calendar* day rather than by 24 hours, which is what keeps
// the fire time stable across a DST transition.
//
// On a spring-forward day the target time may not exist locally (02:30 where the
// clock jumps 02:00→03:00). time.Date normalises that to the equivalent instant,
// so the job fires an hour later on that one day rather than being skipped —
// running late beats not running.
func (d Daily) Next(from time.Time) time.Time {
	next := time.Date(from.Year(), from.Month(), from.Day(), d.Hour, d.Minute, 0, 0, from.Location())
	if !next.After(from) {
		next = time.Date(from.Year(), from.Month(), from.Day()+1, d.Hour, d.Minute, 0, 0, from.Location())
	}
	return next
}

// delay is how long to wait for the next occurrence, measured entirely on
// [Daily.Now]'s clock. Split out from Run so the scheduling arithmetic is
// testable without a real timer.
func (d Daily) delay(now time.Time) time.Duration { return d.Next(now).Sub(now) }

func (d Daily) tick(ctx context.Context) {
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}

	var err error
	if panicked := safe.Do("job:"+d.Name, func() { err = d.Do(ctx) }); panicked {
		return // already logged with a stack trace by safe.Do
	}
	if err == nil {
		return
	}
	if d.OnError != nil {
		d.OnError(ctx, err)
		return
	}
	d.logger().WarnContext(ctx, "daily job failed", "job", d.Name, logx.Err(err))
}

func (d Daily) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Daily) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}
