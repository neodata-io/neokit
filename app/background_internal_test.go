package app

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/neodata-io/neokit/declare"
)

// The bug this change exists to fix. A job still finishing its write when
// SIGTERM arrives must be joined before the store it writes to closes;
// without the join, "store closed" lands first and the write hits a closed
// database. The ordering is the whole reason the step exists.
func TestBackgroundWorkIsJoinedBeforeTheCallersTeardown(t *testing.T) {
	a := newInternalApp(t)

	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); defer mu.Unlock(); order = append(order, s) }

	// Declared first, as a service opens its database first.
	a.ClosesOnShutdown("store", "test", func(context.Context) error {
		record("store closed")
		return nil
	})
	// Declared after it, and deliberately slow to stop: the sleep is what a
	// join has to wait out and a missing one races straight past.
	declare.Add(a, "writer", declare.Run(func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		record("write finished")
	}))

	a.startBackgroundWork()
	a.pushRunSteps()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"write finished", "store closed"}; !slices.Equal(order, want) {
		t.Fatalf("teardown recorded %v, want %v", order, want)
	}
}

// An unconfigured optional feature must not start doing work — the rule Ready
// already follows. Backups with no directory configured is the real case.
func TestBackgroundWorkSkipsComponentsThatAreOff(t *testing.T) {
	a := newInternalApp(t)

	started := make(chan struct{})
	declare.Add(a, "backups",
		declare.Run(func(context.Context) { close(started) }),
		declare.Disabled("no backup directory configured"))

	a.startBackgroundWork()
	a.pushRunSteps()
	_ = a.Close()

	select {
	case <-started:
		t.Error("started the work of a component declared off")
	default:
	}
}

// The application context is what a job selects on, so it has to be the one
// cancelled during teardown rather than a fresh Background().
func TestBackgroundWorkRunsOnTheApplicationContext(t *testing.T) {
	a := newInternalApp(t)

	got := make(chan context.Context, 1)
	declare.Add(a, "writer", declare.Run(func(ctx context.Context) {
		got <- ctx
		<-ctx.Done()
	}))

	a.startBackgroundWork()

	var ctx context.Context
	select {
	case ctx = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("work never started")
	}
	if ctx.Err() != nil {
		t.Fatalf("work started on an already-cancelled context: %v", ctx.Err())
	}

	a.pushRunSteps()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if ctx.Err() == nil {
		t.Error("teardown left the work's context live")
	}
}
