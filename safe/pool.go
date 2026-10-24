package safe

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Pool runs one-shot tasks on a bounded number of goroutines and joins them at
// shutdown. It is the fire-and-forget counterpart of [Group]: a task runs at
// most once — a panic is logged and ends that task, never respawns it — which is
// what work triggered by a request needs (provision this user, send this mail),
// where running it again would repeat a side effect.
//
// Go never blocks the caller. A task past the limit waits on the goroutine it
// was given until a slot frees, or until its context ends. The zero Pool is
// ready to use and unbounded; set Limit for a bound.
type Pool struct {
	// Limit caps how many tasks run at once. Zero means no cap. It is read on the
	// first Go and fixed from then on.
	Limit int
	// Log receives a task's panic and the drain-timeout line. Nil means
	// slog.Default().
	Log *slog.Logger

	// mu guards closed, the lazy semaphore, and the Add below. Add under the same
	// lock as the closed check is what keeps Go off the documented race between
	// a WaitGroup's Add and a Wait already in progress.
	mu     sync.Mutex
	sem    chan struct{}
	wg     sync.WaitGroup
	closed bool
}

func (p *Pool) logger() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// Go runs fn on a new goroutine once a slot is free, and reports whether it was
// accepted. It reports false, and runs nothing, when the pool is draining (Wait
// was called) or ctx is already done: a task started into a shutdown could not
// finish, and would only hold the drain open. The caller decides how loudly to
// say so; it knows what the task was.
//
// fn receives ctx and must honour it — cancellation is how shutdown reaches a
// task that is already running.
func (p *Pool) Go(ctx context.Context, name string, fn func(ctx context.Context)) bool {
	p.mu.Lock()
	if p.closed || ctx.Err() != nil {
		p.mu.Unlock()
		return false
	}
	if p.sem == nil && p.Limit > 0 {
		p.sem = make(chan struct{}, p.Limit)
	}
	sem := p.sem
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		if sem != nil {
			select {
			case sem <- struct{}{}:
				// Both cases can be ready at once when a slot frees as the context
				// ends; select picks at random, so decide on the context, not the luck.
				if ctx.Err() != nil {
					<-sem
					return
				}
			case <-ctx.Done():
				return // shut down while queued: the work can no longer finish
			}
			defer func() { <-sem }()
		}
		runGuarded(p.logger(), name, func() { fn(ctx) })
	}()
	return true
}

// Wait stops the pool accepting tasks and blocks until the running ones return,
// or until ctx — the shutdown step's budget — is done, in which case it returns
// an error wrapping [ErrDrainTimeout]. Unlike [Group.Wait] it is terminal: a
// pool is drained once, at shutdown, and a Go after it is refused.
func (p *Pool) Wait(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		p.logger().Warn("gave up waiting for pool tasks to finish", "cause", ctx.Err())
		return fmt.Errorf("%w: %w", ErrDrainTimeout, ctx.Err())
	}
}
