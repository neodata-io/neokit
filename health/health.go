// Package health answers the two questions an orchestrator asks, and keeps them
// apart.
//
// **Liveness** ("is this process alive") must touch nothing. A liveness probe
// that consults the database gets a healthy container killed during a database
// blip — a restart that cannot possibly fix the database, and that removes
// capacity exactly when the dependency is struggling. [LiveHandler] therefore
// always answers 200.
//
// **Readiness** ("should traffic be sent here") runs the registered checks. A
// failing dependency takes this instance out of rotation without restarting it.
//
// The detail — which check failed, with what error — is what makes readiness
// worth reading and also what you should not hand to strangers, so it is split
// from the verdict rather than traded against it. [Registry.ReadyHandler]
// answers the public probe with the verdict alone; the detail goes to
// [Registry.Log] on every transition, and to [Registry.DetailHandler] for a
// route you put behind your own authentication.
package health

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// CheckFunc reports whether one dependency is usable. It must honour ctx.
type CheckFunc func(ctx context.Context) error

// DefaultTimeout bounds one Check sweep. A readiness probe has a deadline of its
// own, so an unbounded sweep is how a rolling deploy stalls.
const DefaultTimeout = 3 * time.Second

type check struct {
	name string
	fn   CheckFunc

	// mu guards flight, the one call of fn that may be in progress. Overlapping
	// sweeps wait on it rather than each starting their own — see runBounded.
	mu     sync.Mutex
	flight *flight
}

// flight is one call of a check's fn, shared by every sweep that overlaps it.
type flight struct {
	ctx      context.Context // bounded by deadline, independent of any one sweep
	deadline time.Time
	done     chan struct{} // closed once res is set
	res      CheckResult
}

// Registry holds the readiness checks. The zero value is ready to use and safe
// for concurrent use.
type Registry struct {
	// Timeout bounds one full Check. Zero means [DefaultTimeout].
	Timeout time.Duration

	// Log receives the readiness transition lines — which check failed and with
	// what error — that [Registry.ReadyHandler] deliberately keeps out of its
	// body. Nil means [slog.Default].
	//
	// It is where a failure becomes diagnosable now that the endpoint answers
	// with a bare verdict, so a registry with a discarding logger and no
	// [Registry.DetailHandler] mounted anywhere can report "not ready" with no way
	// left to find out why.
	Log *slog.Logger

	mu     sync.RWMutex
	checks []*check

	// state is the last readiness observed, for the transition log. Atomic rather
	// than under mu: it is written from every probe and must not contend with
	// Register, and its Swap is what makes "log only on change" race-free when
	// two probes overlap.
	state atomic.Int32
}

// Register adds a readiness check.
//
// A duplicate name is kept rather than replaced: registering twice is a wiring
// mistake, and silently dropping one would halve the coverage while still
// looking correct in the output.
func (r *Registry) Register(name string, fn CheckFunc) {
	if fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, &check{name: name, fn: fn})
}

// Len reports how many checks are registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.checks)
}

// CheckResult is one dependency's answer.
type CheckResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Err  string `json:"error,omitempty"`

	// Took is the check's duration for Go callers.
	//
	// It is not serialised directly: time.Duration marshals as its raw int64
	// nanoseconds, so a field named "tookMs" carrying 80000000 would be wrong by
	// six orders of magnitude. TookMs below is what goes on the wire.
	Took time.Duration `json:"-"`

	// TookMs is Took in whole milliseconds, which is the resolution a readiness
	// probe is actually read at.
	TookMs int64 `json:"tookMs"`
}

// Result is the whole sweep. A registry with no checks is ready: a service with
// no dependencies must not be permanently unready because nobody registered
// anything.
type Result struct {
	Ready  bool          `json:"ready"`
	Checks []CheckResult `json:"checks"`
}

// Check runs every registered check concurrently under one bounded context.
//
// The sweep returns at the deadline whether or not every check has answered: one
// that ignores ctx is reported as failed and left to finish on its own, so a
// wedged dependency cannot hold the readiness endpoint open. A check is never
// entered twice concurrently: sweeps that overlap share one call and its result.
func (r *Registry) Check(ctx context.Context) Result {
	r.mu.RLock()
	checks := make([]*check, len(r.checks))
	copy(checks, r.checks)
	r.mu.RUnlock()

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	results := make([]CheckResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Go(func() {
			results[i] = runBounded(ctx, c, deadline)
		})
	}
	wg.Wait()

	out := Result{Ready: true, Checks: results}
	for _, res := range results {
		if !res.OK {
			out.Ready = false
		}
	}
	return out
}

// runBounded waits for c's answer no longer than ctx allows, and keeps at most
// one call of c.fn in flight: an overlapping sweep joins it, and only a call
// past its own deadline is reported as stuck — never started a second time.
func runBounded(ctx context.Context, c *check, deadline time.Time) CheckResult {
	started := time.Now()

	c.mu.Lock()
	f := c.flight
	if f == nil {
		// Detached from the sweep that happens to start it: one prober hanging up
		// must not fail the check for the others waiting on the same call.
		fctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		f = &flight{ctx: fctx, deadline: deadline, done: make(chan struct{})}
		c.flight = f
		go func() {
			defer cancel()
			res := run(fctx, c)
			c.mu.Lock()
			f.res = res
			c.flight = nil
			c.mu.Unlock()
			close(f.done)
		}()
	} else if !started.Before(f.deadline) {
		c.mu.Unlock()
		return notAnswered(c.name, "still running from an earlier check", time.Since(started))
	}
	c.mu.Unlock()

	var reason error
	select {
	case <-f.done:
		return f.res
	case <-ctx.Done():
		reason = ctx.Err()
	case <-f.ctx.Done():
		reason = f.ctx.Err()
	}
	// The call's context is released as it finishes, so both can be ready at
	// once; an answer that arrived wins over the luck of the select.
	select {
	case <-f.done:
		return f.res
	default:
		return notAnswered(c.name, reason.Error(), time.Since(started))
	}
}

// notAnswered is the result for a check that did not report within the sweep.
// took is how long readiness waited, which is not how long the check has been
// running — that is the point of reporting it.
func notAnswered(name, reason string, took time.Duration) CheckResult {
	return CheckResult{Name: name, Err: reason, Took: took, TookMs: took.Milliseconds()}
}

// run executes one check, converting a panic into a failure.
//
// A panicking check is a bug in that check, not a reason to take the endpoint
// down: an unrecovered panic here is a 500 with no body, at exactly the moment
// an operator is asking what is wrong.
func run(ctx context.Context, c *check) (res CheckResult) {
	res = CheckResult{Name: c.name}
	start := time.Now()
	defer func() {
		res.Took = time.Since(start)
		res.TookMs = res.Took.Milliseconds()
		if p := recover(); p != nil {
			res.OK = false
			res.Err = fmt.Sprintf("check panicked: %v", p)
		}
	}()

	if err := c.fn(ctx); err != nil {
		res.OK = false
		res.Err = err.Error()
		return res
	}
	res.OK = true
	return res
}
