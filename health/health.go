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
// A readiness body names each check and its error, which is what makes it worth
// reading and also the reason to think about where you serve it: on the
// application port — where neokit's app builder mounts it — that detail is
// public, and the listener's bind address is the only thing narrowing it.
package health

import (
	"context"
	"fmt"
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

	// running is held for as long as fn has not returned. It is what caps the
	// cost of a check that ignores its context — see runBounded.
	running atomic.Bool
}

// Registry holds the readiness checks. The zero value is not usable; construct
// with [New]. Safe for concurrent use.
type Registry struct {
	// Timeout bounds one full Check. Zero means [DefaultTimeout].
	Timeout time.Duration

	mu     sync.RWMutex
	checks []*check
}

func New() *Registry { return &Registry{} }

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
// entered twice concurrently — see runBounded.
func (r *Registry) Check(ctx context.Context) Result {
	r.mu.RLock()
	checks := make([]*check, len(r.checks))
	copy(checks, r.checks)
	r.mu.RUnlock()

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	results := make([]CheckResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runBounded(ctx, c)
		}()
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

// runBounded does not wait beyond ctx's deadline, and never leaves more than one
// call of c.fn in flight.
//
// Go cannot stop a callback that ignores its context. Handing the call to a
// goroutine with a buffered result channel is what lets the readiness request
// answer on time and lets the callback finish whenever it eventually does. On
// its own, though, that trades a hang for a leak: readiness is polled on a
// timer, so a permanently wedged dependency would strand a fresh goroutine —
// and everything its check closed over — every few seconds for the life of the
// process. The running flag bounds the wedged case to one stranded goroutine in
// total, and says so in the result: "still running" tells an operator the
// dependency is stuck rather than slow, which a repeated deadline error does not.
func runBounded(ctx context.Context, c *check) CheckResult {
	started := time.Now()
	if !c.running.CompareAndSwap(false, true) {
		return notAnswered(c.name, "still running from an earlier check", time.Since(started))
	}

	result := make(chan CheckResult, 1)
	go func() {
		res := run(ctx, c)
		// Released before the send, so the sweep that receives res can never
		// observe the flag still held and report a finished check as wedged.
		c.running.Store(false)
		result <- res
	}()

	select {
	case res := <-result:
		return res
	case <-ctx.Done():
		return notAnswered(c.name, ctx.Err().Error(), time.Since(started))
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
