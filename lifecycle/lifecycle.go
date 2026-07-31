// Package lifecycle owns process startup and shutdown: a context cancelled by
// the usual termination signals, and an ordered stack of teardown steps.
//
// Teardown has to run in reverse of the order things were built, because a step
// can only be torn down while everything depending on it is still alive — stop
// accepting requests before closing the database the in-flight ones are reading.
// [Stack] enforces exactly that (LIFO) and nothing else: it starts nothing, owns
// no goroutines, and decides no dependencies. Push a step right after building
// the thing it tears down, so the ordering is readable at the call site.
//
// Each step is bounded so one wedged Close cannot hold the process open, a
// panicking step cannot skip the steps below it, every outcome and duration is
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
// It is [signal.NotifyContext] with that set filled in: a server handling SIGINT
// but not SIGTERM shuts down cleanly on ^C and is killed uncleanly by every
// orchestrator.
//
// Call the returned stop function (usually deferred) to release the handler and
// restore the default disposition, after which a second signal terminates the
// process immediately — what an operator pressing ^C twice expects.
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
// A nil fn is ignored rather than deferred into a panic at shutdown: the natural
// `stack.Push("store", store.Close)` on a nil store is a wiring mistake worth
// surviving, since the alternative crashes the one path that must not crash.
//
// Pushing after [Stack.Shutdown] is a no-op and is logged — the step would never
// run, and dropping it silently would leave a resource open with nothing to say so.
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

// Names returns the registered step names in push order. Shutdown runs them in
// reverse, so this is the teardown order read backwards.
//
// It exists because push order is a correctness property — a step pushed in the
// wrong place closes a resource while something still depends on it — and a
// caller that cannot inspect it can only assert the order it typed itself.
func (s *Stack) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.steps))
	for i, st := range s.steps {
		out[i] = st.name
	}
	return out
}

// Shutdown runs every registered step in reverse order, bounding each one by
// each (zero means "only ctx bounds it"), and returns every error joined.
//
// It runs the steps **sequentially**, which is the point: concurrent teardown
// would discard the ordering that is this type's entire reason to exist.
//
// A step that exceeds its budget does not stop the sweep — otherwise a process
// holds its database handle because an HTTP server was slow to drain, and the
// step that hung is the one whose resource the OS reclaims at exit anyway.
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
