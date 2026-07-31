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
// Serve both on a diagnostics listener rather than the application port, so they
// inherit its binding and never widen the public surface.
package health

import (
	"context"
	"fmt"
	"sync"
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
}

// Registry holds the readiness checks. The zero value is not usable; construct
// with [New]. Safe for concurrent use.
type Registry struct {
	// Timeout bounds one full Check. Zero means [DefaultTimeout].
	Timeout time.Duration

	mu     sync.RWMutex
	checks []check
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
	r.checks = append(r.checks, check{name: name, fn: fn})
}

// Len reports how many checks are registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.checks)
}

// CheckResult is one dependency's answer.
type CheckResult struct {
	Name string        `json:"name"`
	OK   bool          `json:"ok"`
	Err  string        `json:"error,omitempty"`
	Took time.Duration `json:"tookMs"`
}

// Result is the whole sweep. A registry with no checks is ready: a service with
// no dependencies must not be permanently unready because nobody registered
// anything.
type Result struct {
	Ready  bool          `json:"ready"`
	Checks []CheckResult `json:"checks"`
}

// Check runs every registered check concurrently under one bounded context.
func (r *Registry) Check(ctx context.Context) Result {
	r.mu.RLock()
	checks := make([]check, len(r.checks))
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
			results[i] = run(ctx, c)
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

// run executes one check, converting a panic into a failure.
//
// A panicking check is a bug in that check, not a reason to take the endpoint
// down: an unrecovered panic here is a 500 with no body, at exactly the moment
// an operator is asking what is wrong.
func run(ctx context.Context, c check) (res CheckResult) {
	res = CheckResult{Name: c.name}
	start := time.Now()
	defer func() {
		res.Took = time.Since(start)
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
