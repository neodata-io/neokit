package safe_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/neodata-io/neokit/safe"
)

// drainCtx is the shutdown budget a caller of WaitGo actually holds.
func drainCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// The package-level pair has to reach the same default group, or app.Run's
// drain joins nothing that jobs.Job.Start spawned. Asserted end to end because
// defaultGroup is unexported.
//
// Each test here shares that process-wide group, so every goroutine one starts
// must be drained before it returns — a leaked straggler makes the next test's
// assertion about its own work instead.
func TestGoAndWaitGoShareTheDefaultGroup(t *testing.T) {
	ran := make(chan struct{})
	safe.Go(context.Background(), "worker", func() { close(ran) })

	if err := safe.WaitGo(drainCtx(t, 5*time.Second)); err != nil {
		t.Fatalf("WaitGo: %v", err)
	}
	select {
	case <-ran:
	default:
		t.Error("WaitGo returned before the goroutine Go started had run")
	}
}

// A drain that gave up has to say so: app.Run turns it into a failed shutdown
// step, and a process that abandoned background work must not exit zero.
func TestWaitGoReportsATimeout(t *testing.T) {
	release := make(chan struct{})
	// Drain the straggler before leaving, or the next test's WaitGo waits on it.
	t.Cleanup(func() { close(release); safe.WaitGo(drainCtx(t, 5*time.Second)) })

	safe.Go(context.Background(), "straggler", func() { <-release })

	if err := safe.WaitGo(drainCtx(t, 50*time.Millisecond)); !errors.Is(err, safe.ErrDrainTimeout) {
		t.Errorf("WaitGo err = %v, want %v", err, safe.ErrDrainTimeout)
	}
}

// A cancelled budget must end the drain too. The step context app.Run passes is
// cancelled — not only expired — when an earlier step has already used the
// grace period up, and a Wait that only watched a timer would sit there anyway.
func TestWaitGoStopsOnACancelledContext(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release); safe.WaitGo(drainCtx(t, 5*time.Second)) })

	safe.Go(context.Background(), "straggler", func() { <-release })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := safe.WaitGo(ctx); !errors.Is(err, safe.ErrDrainTimeout) {
		t.Errorf("WaitGo err = %v, want %v", err, safe.ErrDrainTimeout)
	}
}

func TestGoRecoversAPanicAndKeepsTheCountBalanced(t *testing.T) {
	// If the panic escaped its goroutine it would take the whole test binary
	// down, so reaching the next line is half the assertion.
	//
	// An already-done context is what keeps the goroutine out of the shared
	// default group afterwards: this fn panics on every call, so under a live
	// context it would respawn for the rest of the binary's life.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	safe.Go(ctx, "panicking-worker", func() { panic("boom") })
	safe.WaitGo(drainCtx(t, 5*time.Second))

	// The non-obvious failure mode: a recovered panic that skipped its deferred
	// leave() would leave the group's count unbalanced, and every later WaitGo
	// would block until its budget ran out.
	ran := make(chan struct{})
	safe.Go(context.Background(), "second-worker", func() { close(ran) })
	safe.WaitGo(drainCtx(t, 5*time.Second))

	select {
	case <-ran:
	default:
		t.Error("second Go did not complete: the group's count was left unbalanced")
	}
}
