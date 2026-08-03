package app

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/safe"
)

func newInternalApp(t *testing.T) *App {
	t.Helper()

	a, err := New(Options{
		Name: "testapp", Version: "1.2.3",
		Base: config.Base{Port: 0, LogLevel: "error", LogFormat: "json"},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// The bug this change exists to fix. A job still finishing its write when
// SIGTERM arrives must be joined before the store it writes to closes;
// without the join, "store closed" lands first and the write hits a closed
// database. The ordering is the whole reason the step exists.
func TestBackgroundWorkIsJoinedBeforeTheCallersTeardown(t *testing.T) {
	a := newInternalApp(t)

	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); defer mu.Unlock(); order = append(order, s) }

	// Pushed first, as a service opens its database first.
	a.Shutdown.Push("store", func(context.Context) error {
		record("store closed")
		return nil
	})
	// Started after it, and deliberately slow to stop: the sleep is what a join
	// has to wait out and a missing one races straight past.
	safe.Go(a.Context(), "writer", func() {
		<-a.Context().Done()
		time.Sleep(50 * time.Millisecond)
		record("write finished")
	})

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

// The application context is what a job selects on, so it has to be the one
// cancelled during teardown rather than a fresh Background().
func TestBackgroundWorkRunsOnTheApplicationContext(t *testing.T) {
	a := newInternalApp(t)

	got := make(chan context.Context, 1)
	safe.Go(a.Context(), "writer", func() {
		got <- a.Context()
		<-a.Context().Done()
	})

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
