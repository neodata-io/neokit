package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/lifecycle"
)

// fakeStore stands in for a dependency with the Close() error shape.
type fakeStore struct{ closed bool }

func (s *fakeStore) Close() error { s.closed = true; return nil }

// The teardown half of a declaration has to actually run, or the single call
// that replaced PushCloser + Declare has quietly dropped the teardown.
func TestComponentCloseRunsOnShutdown(t *testing.T) {
	a := newApp(t)
	store := &fakeStore{}

	a.Declare(app.Component{
		Name: "database", On: true, Detail: "/tmp/test.db",
		Close: lifecycle.Closer(store),
	})

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !store.closed {
		t.Error("the declared Close never ran")
	}
}

// Close runs even when the component is off, unlike Ready. A non-nil Close means
// something was allocated, and skipping it because a feature reports off leaks it.
func TestComponentCloseRunsWhenTheComponentIsOff(t *testing.T) {
	a := newApp(t)

	closed := false
	a.Declare(app.Component{
		Name: "backups", On: false, Detail: "disabled",
		Close: func(context.Context) error { closed = true; return nil },
	})

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Error("Close was skipped because On was false; it must still run")
	}
}

// Declaration order is push order and the stack unwinds in reverse, so the
// dependency declared last is released first — the property Push exists for.
func TestComponentCloseUnwindsInReverseDeclarationOrder(t *testing.T) {
	a := newApp(t)

	var order []string
	for _, name := range []string{"database", "cache", "bus"} {
		a.Declare(app.Component{
			Name: name, On: true,
			Close: func(context.Context) error {
				order = append(order, name)
				return nil
			},
		})
	}

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []string{"bus", "cache", "database"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// A report-only component must register no step: Declare still has to serve the
// things that hold nothing, which is most of neokit's own declarations.
func TestComponentWithoutCloseDoesNotTouchTheStack(t *testing.T) {
	a := newApp(t)

	// New has already pushed its own steps (tracing, metrics-export), so this
	// compares against that baseline rather than an absolute count.
	before := a.Shutdown.Len()
	a.Declare(app.Component{Name: "report-only", On: true, Detail: "nothing to close"})

	if got := a.Shutdown.Len(); got != before {
		t.Errorf("stack grew from %d to %d; a nil Close must push nothing", before, got)
	}
}
