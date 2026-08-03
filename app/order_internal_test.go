package app

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/neodata-io/neokit/config"
)

// The teardown order is a correctness property produced by the order
// pushRunSteps pushes — not by anything a test can restate. This exercises the
// real method, so reordering those three pushes fails here rather than in
// production, where the symptom is a socket closed under a live caller.
func TestPushRunStepsProducesTheDocumentedOrder(t *testing.T) {
	a, err := New(Options{
		Name: "testapp", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Base: config.Base{LogLevel: "error", LogFormat: "json"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	// An application's own step, pushed between New's and Run's, as a real
	// caller does.
	a.Shutdown.Push("database", func(context.Context) error { return nil })
	a.pushRunSteps()

	teardown := slices.Clone(a.Shutdown.Names())
	slices.Reverse(teardown) // Shutdown unwinds in reverse of push order

	want := []string{
		"streams", "api", "background-context", "background-work",
		"database", "metrics-export", "tracing",
	}
	if !slices.Equal(teardown, want) {
		t.Fatalf("teardown order = %v\nwant                %v", teardown, want)
	}

	at := func(name string) int { return slices.Index(teardown, name) }
	// Each prevents one specific failure; assert by name so a future reorder
	// says which invariant it broke.
	if at("streams") > at("api") {
		t.Error(`"streams" must precede "api", or the drain waits out its timeout on a live stream`)
	}
	if at("background-context") < at("api") {
		t.Error(`"background-context" must follow "api", or a late request starts work the drain is waiting for`)
	}
	if at("database") < at("api") {
		t.Error(`the application's own steps must follow "api", or a query hits a closed store`)
	}
	if at("background-work") < at("background-context") {
		t.Error(`"background-work" must follow "background-context", or the join waits on work nothing has told to stop`)
	}
	if at("background-work") > at("database") {
		t.Error(`"background-work" must precede the application's own steps, or a job's store closes mid-write`)
	}
	if at("tracing") != len(teardown)-1 {
		t.Error(`"tracing" must be last, so a span from any earlier step still exports`)
	}
}

// The drain signal is unexported, so this is the only level it can be asserted
// at directly. [TestStreamContextEndsOnDrain] covers the same property through
// the public API and a real listener; this one pins the mechanism, and that a
// Close reaching it twice cannot panic at the worst possible moment.
func TestCloseReleasesTheDrainSignal(t *testing.T) {
	a, err := New(Options{
		Name: "testapp", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Base: config.Base{LogLevel: "error", LogFormat: "json"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	select {
	case <-a.shuttingDown.ctx.Done():
		t.Fatal("the drain signal must stay open while the app runs")
	default:
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-a.shuttingDown.ctx.Done():
	default:
		t.Error("Close must release the drain signal")
	}

	// Run's "streams" step and a caller's deferred Close both reach
	// signalShutdown. The old channel-close needed a sync.Once to survive this.
	a.signalShutdown()
}
