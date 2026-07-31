package safe

import (
	"errors"
	"log/slog"
	"sync"
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
// sync.WaitGroup, and that is a correctness requirement, not a style choice.
// WaitGroup requires that an Add taking the counter off zero happens-before any
// Wait. Go and Wait are independent exported entry points, so nothing could
// enforce that ordering, and a process that spawned background work while a
// drain was joining died on:
//
//	panic: sync: WaitGroup is reused before previous Wait has returned
//
// That panic was thrown from inside the goroutine Wait itself spawned, which
// put it beyond the reach of any recover the caller could install — so the
// package whose whole purpose is stopping a panic from killing the process
// killed the process. A counter guarded by the same mutex Wait reads under has
// no such ordering rule, and needs no helper goroutine, so a drain that times
// out no longer leaves one blocked forever either.
//
// The zero Group is ready to use, and a Group may be drained more than once.
// Prefer one Group per subsystem: its Wait joins that Group's goroutines and no
// one else's, which is the property the package-level default cannot offer.
type Group struct {
	mu sync.Mutex
	n  int // supervised goroutines currently live

	// idle is non-nil exactly while n > 0, and is closed when n falls back to
	// zero. Wait reads it under mu and then selects on it, so a goroutine
	// finishing concurrently is never missed.
	idle chan struct{}

	// Log receives supervision events (a panic, a restart, a drain that timed
	// out). Nil means slog.Default(). A library should not force its
	// diagnostics into a process-wide logger the caller cannot redirect.
	Log *slog.Logger
}

func (g *Group) logger() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

func (g *Group) enter() {
	g.mu.Lock()
	if g.n == 0 {
		g.idle = make(chan struct{})
	}
	g.n++
	g.mu.Unlock()
}

func (g *Group) leave() {
	g.mu.Lock()
	g.n--
	if g.n == 0 && g.idle != nil {
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
func (g *Group) Go(name string, fn func()) {
	g.enter()
	go func() {
		defer g.leave()
		for {
			if !runGuarded(g.logger(), name, fn) {
				return // fn returned normally — nothing to respawn
			}
			g.logger().Warn("restarting panicked background goroutine", "goroutine", name)
			time.Sleep(restartBackoff)
		}
	}()
}

// Wait blocks until every goroutine in the group has returned — which happens
// once their context is cancelled at shutdown — or until timeout elapses.
// Shutdown calls it before closing the resources those goroutines use, so a job
// mid-call isn't cut off by having its connection closed underneath it.
//
// It returns [ErrDrainTimeout] if the drain did not finish. The caller is about
// to tear down the very resources the stragglers are still using, so "some are
// still running" is a decision for the caller to make rather than a warning to
// bury — the previous version returned normally and only logged, which silently
// reintroduced the race the drain exists to prevent.
//
// Wait does not stop the group: goroutines may be supervised again afterwards,
// and Wait may be called repeatedly.
func (g *Group) Wait(timeout time.Duration) error {
	g.mu.Lock()
	idle := g.idle
	g.mu.Unlock()

	if idle == nil {
		return nil // nothing running
	}

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-idle:
		return nil
	case <-t.C:
		g.logger().Warn("timed out waiting for background goroutines to stop", "timeout", timeout)
		return ErrDrainTimeout
	}
}

// Len reports how many supervised goroutines are currently live. It exists for
// a shutdown path that wants to say how many stragglers it is abandoning.
func (g *Group) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// defaultGroup backs the package-level Go and WaitGo.
//
// It is process-wide state, with the coupling that implies: a WaitGo on it
// drains every caller's goroutines, including a dependency's. That is tolerable
// in a binary's own composition root, which is the only place it should be
// used — anything reusable should hold its own [Group].
var defaultGroup Group

// Go supervises fn on the package-level default group. See [Group.Go].
func Go(name string, fn func()) { defaultGroup.Go(name, fn) }

// WaitGo drains the package-level default group. See [Group.Wait].
//
// The error is dropped to keep the historical signature; a timeout is still
// logged. Use [Group.Wait] to act on it.
func WaitGo(timeout time.Duration) { _ = defaultGroup.Wait(timeout) }

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
