// Package lifecycle owns process startup and shutdown: a context cancelled by
// the usual termination signals, and an ordered stack of teardown steps.
//
// Shutdown order is the part that is hard, and the part a framework must not
// hide. Teardown has to run in the reverse of the order things were built,
// because a step can only be torn down while everything that depends on it is
// still alive — stop accepting requests before you close the database the
// in-flight ones are reading. [Stack] enforces exactly that (LIFO) and nothing
// else: it does not start anything, does not own your goroutines, and does not
// decide what depends on what. You push a step when you have finished building
// the thing it tears down, right next to the thing itself, so the ordering is
// readable at the call site rather than encoded in a dependency graph.
//
// Everything that makes a hand-written shutdown subtly wrong is handled: each
// step is bounded so one wedged Close cannot hold the process open, a panicking
// step cannot skip the steps below it, every step's outcome and duration is
// logged, and every error is collected rather than the first one winning.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/safe"
)

// Signals returns a context cancelled on SIGINT or SIGTERM — the two a
// container runtime and a terminal actually send.
//
// It is [signal.NotifyContext] with that set filled in, which is the only part
// worth sharing: the stdlib call is easy to reach for with an incomplete set,
// and a server that handles SIGINT but not SIGTERM shuts down cleanly when you
// press ^C and is killed uncleanly by every orchestrator on earth.
//
// Call the returned stop function (usually deferred) to release the signal
// handler and restore the default disposition — after which a second signal
// terminates the process immediately, which is the behaviour an operator
// pressing ^C twice expects.
func Signals(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
}

// SignalsWith is [Signals] with an explicit signal set, for a process that also
// wants SIGHUP or similar.
func SignalsWith(parent context.Context, sig ...os.Signal) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, sig...)
}

// Step is one teardown action. It must honour ctx: that is how [Stack.Shutdown]
// bounds it.
type Step func(ctx context.Context) error

type step struct {
	name string
	fn   Step
}

// Stack collects teardown steps and runs them in reverse order. The zero Stack
// is ready to use, and is safe for concurrent Push.
type Stack struct {
	mu    sync.Mutex
	steps []step
	done  bool

	// Log receives per-step outcomes. Nil means slog.Default().
	Log *slog.Logger
}

// Push registers a teardown step. Steps run in reverse registration order, so
// push each one immediately after building the resource it releases.
//
// A nil fn is ignored rather than deferred into a panic at shutdown — the
// natural call `stack.Push("store", store.Close)` on a store that turned out to
// be nil is a wiring mistake worth surviving, since the alternative is a crash
// during the one code path that must not crash.
//
// Pushing after [Stack.Shutdown] has run is a no-op and is logged: the step
// would never run, and silently dropping it would leave a resource open with
// nothing to say so.
func (s *Stack) Push(name string, fn Step) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		s.logger().Warn("shutdown step registered after shutdown; it will never run", "step", name)
		return
	}
	s.steps = append(s.steps, step{name: name, fn: fn})
}

// PushCloser adapts an ordinary Close() error — the shape most libraries expose
// — into a step.
func (s *Stack) PushCloser(name string, closer interface{ Close() error }) {
	if closer == nil {
		return
	}
	s.Push(name, func(context.Context) error { return closer.Close() })
}

// Len reports how many steps are registered.
func (s *Stack) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.steps)
}

// Shutdown runs every registered step in reverse order, bounding each one by
// each (zero means "only ctx bounds it"), and returns every error joined.
//
// It runs the steps **sequentially**, which is the point: concurrent teardown
// would discard the ordering that is this type's entire reason to exist.
//
// A step that exceeds its budget does not stop the sweep — the remaining steps
// still run. That is deliberate: the alternative leaves a process holding a
// database handle because an HTTP server was slow to drain, and the step that
// hung is exactly the one whose resource the OS will reclaim at exit anyway.
//
// Shutdown is idempotent: a second call does nothing and returns nil.
func (s *Stack) Shutdown(ctx context.Context, each time.Duration) error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	s.done = true
	steps := s.steps
	s.steps = nil
	s.mu.Unlock()

	log := s.logger()
	var errs []error
	for i := len(steps) - 1; i >= 0; i-- {
		st := steps[i]
		start := time.Now()
		err := runStep(ctx, st, each)
		took := time.Since(start)

		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", st.name, err))
			log.Error("shutdown step failed", "step", st.name, "took", took, logx.Err(err))
		default:
			log.Info("shutdown step complete", "step", st.name, "took", took)
		}
	}
	return errors.Join(errs...)
}

// runStep bounds and guards one step.
//
// The panic guard matters more here than anywhere else in a program: a panic in
// a teardown function would otherwise unwind out of Shutdown and skip every step
// below it — losing the database flush because the metrics server misbehaved.
func runStep(ctx context.Context, st step, each time.Duration) (err error) {
	if each > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, each)
		defer cancel()
	}
	if panicked := safe.Do("shutdown:"+st.name, func() { err = st.fn(ctx) }); panicked {
		return errors.New("panicked during shutdown")
	}
	return err
}

func (s *Stack) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
