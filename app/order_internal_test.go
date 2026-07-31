package app

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/neodata-io/neokit/config"
)

// stubDebug stands in for the diagnostics server, which pushRunSteps needs only
// a Shutdown from.
type stubDebug struct{}

func (stubDebug) Shutdown(context.Context) error { return nil }

// The teardown order is a correctness property produced by the order
// pushRunSteps pushes — not by anything a test can restate. This exercises the
// real method, so reordering those four pushes fails here rather than in
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
	a.pushRunSteps(stubDebug{})

	teardown := slices.Clone(a.Shutdown.Names())
	slices.Reverse(teardown) // Shutdown unwinds in reverse of push order

	want := []string{
		"streams", "api", "background-context", "metrics-server",
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
	if at("tracing") != len(teardown)-1 {
		t.Error(`"tracing" must be last, so a span from any earlier step still exports`)
	}
}
