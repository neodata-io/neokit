package safe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ErrDrainTimeout reports that a [Group.Wait] gave up before every supervised
// goroutine had returned.
var ErrDrainTimeout = errors.New("safe: timed out waiting for background goroutines to stop")

// restartBackoff spaces out respawns so a goroutine that panics immediately and
// repeatedly can't hot-loop; it still recovers within seconds of a transient one.
const restartBackoff = 5 * time.Second

// Group supervises a set of background goroutines and joins them at shutdown.
//
// Membership is tracked with a counter and a broadcast channel rather than a
// sync.WaitGroup, and that is a correctness requirement: WaitGroup needs an Add
// taking the counter off zero to happen-before any Wait, but Go and Wait are
// independent exported entry points, so nothing can enforce that ordering.
// Spawning into a group while a drain is joining would panic from inside
// WaitGroup's own helper goroutine, out of reach of any caller's recover.
//
// The zero Group is ready to use, and a Group may be drained more than once.
// Prefer one Group per subsystem: its Wait joins that Group's goroutines and no
// one else's, which the package-level default cannot offer.
type Group struct {
	// n counts live supervised goroutines. Atomic rather than mu-guarded so that
	// spawning stays cheap: taking mu on every Go doubles the cost of a parallel
	// spawn, since every goroutine start contends on one lock. mu guards only
	// idle, which is touched once per drain and once when the count hits zero.
	n atomic.Int64

	mu sync.Mutex

	// idle is created by Wait and closed when n falls back to zero. It is
	// allocated lazily, on the first Wait that finds work in flight, rather than
	// on every 0→1 transition, which would put an allocation on every Go. Wait
	// reads it under mu and then selects on it, so a goroutine finishing
	// concurrently is never missed.
	idle chan struct{}

	// Log receives supervision events (a panic, a restart, a drain that timed
	// out). Nil means slog.Default(). A library should not force its diagnostics
	// into a process-wide logger the caller cannot redirect.
	Log *slog.Logger
}

func (g *Group) logger() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

func (g *Group) enter() { g.n.Add(1) }

// leave decrements the live count and, when it reaches zero, wakes any waiter.
func (g *Group) leave() {
	if g.n.Add(-1) != 0 {
		return
	}
	g.wake()
}

// wake releases a waiter, but only while the group is genuinely idle.
//
// The re-check under mu is the point. leave's decrement and this lock are two
// steps, and a Go landing between them puts the count back above zero: closing
// idle then would report a finished drain to a caller whose very next act is to
// close the resources the new goroutine is using. Nothing is lost by declining
// — whichever leave observes a zero count under mu does the closing.
func (g *Group) wake() {
	g.mu.Lock()
	if g.idle != nil && g.n.Load() == 0 {
		close(g.idle)
		g.idle = nil
	}
	g.mu.Unlock()
}

// Go runs fn in a new goroutine and keeps it alive: a panic in best-effort
// background work is logged and the goroutine respawns after a short backoff,
// rather than crashing the process or — the trap a plain one-shot recover falls
// into — silently dying so that one subsystem stops for the rest of the process
// lifetime with only a single log line. A clean return (fn finished, e.g. its
// context was cancelled at shutdown) ends the loop.
//
// ctx governs the supervision, not fn — fn still has to honour its own
// cancellation to return. What it stops is the respawn into a shutdown: that run
// cannot finish the work, and it does hold the drain open. Pass
// context.Background() only for work that genuinely outlives every caller.
func (g *Group) Go(ctx context.Context, name string, fn func()) {
	g.enter()
	go func() {
		defer g.leave()
		for {
			if !runGuarded(g.logger(), name, fn) {
				return // fn returned normally — nothing to respawn
			}
			if !g.backoff(ctx, name) {
				return // shutting down
			}
		}
	}()
}

// backoff spaces out a respawn and reports whether to make one.
func (g *Group) backoff(ctx context.Context, name string) bool {
	if ctx.Err() != nil {
		g.logger().Warn("not restarting panicked background goroutine: shutting down", "goroutine", name)
		return false
	}
	g.logger().Warn("restarting panicked background goroutine", "goroutine", name)

	t := time.NewTimer(restartBackoff)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Wait blocks until every goroutine in the group has returned — which happens
// once their context is cancelled at shutdown — or until ctx is done. Shutdown
// calls it before closing the resources those goroutines use, so a job mid-call
// isn't cut off by having its connection closed underneath it.
//
// A context rather than a duration: the caller holding a drain budget is a
// shutdown step, and deriving a duration back out of its context loses both the
// early cancellation and the floor at zero.
//
// It returns an error wrapping [ErrDrainTimeout] if the drain did not finish.
// The caller is about to tear down the very resources the stragglers are still
// using, so that is a decision for the caller rather than a warning to bury in a
// log.
//
// Wait does not stop the group: goroutines may be supervised again afterwards,
// and Wait may be called repeatedly.
func (g *Group) Wait(ctx context.Context) error {
	g.mu.Lock()
	if g.n.Load() == 0 {
		g.mu.Unlock()
		return nil // nothing running
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	idle := g.idle
	g.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		g.logger().Warn("gave up waiting for background goroutines to stop",
			"stragglers", g.Len(), "cause", ctx.Err())
		return fmt.Errorf("%w: %w", ErrDrainTimeout, ctx.Err())
	}
}

// Len reports how many supervised goroutines are currently live. It exists for
// a shutdown path that wants to say how many stragglers it is abandoning.
func (g *Group) Len() int { return int(g.n.Load()) }

// defaultGroup backs the package-level Go and WaitGo.
//
// It is process-wide state, with the coupling that implies: a WaitGo on it
// drains every caller's goroutines, including a dependency's. That is tolerable
// in a binary's own composition root, and app.Run is the sanctioned one — it
// owns the signal handlers and the teardown order, so its drain is the process's
// drain. Anything else reusable should hold its own [Group].
var defaultGroup Group

// Go supervises fn on the package-level default group, bounded by ctx. See
// [Group.Go].
func Go(ctx context.Context, name string, fn func()) { defaultGroup.Go(ctx, name, fn) }

// WaitGo drains the package-level default group. See [Group.Wait].
func WaitGo(ctx context.Context) error { return defaultGroup.Wait(ctx) }

// runGuarded runs fn, recovering and logging any panic; it reports whether one
// occurred so the supervisor knows to respawn (true) or stop (false).
func runGuarded(log *slog.Logger, name string, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			logPanic(log, name, r)
		}
	}()
	fn()
	return false
}
