package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/declare"
)

// The whole point of the declare package: a constructor can take a Declarer and
// be handed an *App. Breaking this breaks every self-declaring constructor.
var _ declare.Declarer = (*App)(nil)

// The readiness registry is unexported — reachable only by declaring a
// component — so what Declare feeds into it can only be asserted from inside the
// package. The coupling itself is the contract worth pinning: one Declare, both
// outputs, or a component ends up in the report under one name and in /readyz
// under another.
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

func declared(a *App, name string) (Component, bool) {
	for _, s := range a.Components() {
		if s.Name == name {
			return s, true
		}
	}
	return Component{}, false
}

// One Declare feeds both the boot report and the readiness set, so a component
// is named once.
func TestDeclareRegistersAReadinessCheck(t *testing.T) {
	a := newInternalApp(t)
	a.Declare(Component{
		Name: "database", On: true, Detail: "./data/app.db",
		Ready: func(context.Context) error { return nil },
	})

	if a.checks.Len() != 1 {
		t.Errorf("readiness has %d checks, want the declared one", a.checks.Len())
	}
	got, ok := declared(a, "database")
	if !ok {
		t.Fatalf("database missing from Components(): %+v", a.Components())
	}
	if !got.On || got.Detail != "./data/app.db" {
		t.Errorf("Component = %+v", got)
	}
}

// An unconfigured optional feature must never make a container look unready —
// that would pull a working instance out of rotation over a feature nobody
// asked for.
func TestAnOffComponentRegistersNoCheck(t *testing.T) {
	a := newInternalApp(t)
	a.Declare(Component{
		Name: "login", On: false, Detail: "not configured",
		Ready: func(context.Context) error { return errors.New("never called") },
	})

	if a.checks.Len() != 0 {
		t.Error("an off component must contribute no readiness check")
	}
	if got := a.checks.Check(context.Background()); !got.Ready {
		t.Error("an off component must not make the app unready")
	}
}

// A late declaration is silently useless: the report is already printed and
// startBackgroundWork has already iterated the components, so its Run never
// starts. The warning is the only trace it leaves.
func TestALateDeclarationWarns(t *testing.T) {
	var log strings.Builder
	a := newInternalApp(t)
	a.Log = slog.New(slog.NewTextHandler(&log, nil))

	a.started.Store(true) // as Run sets it, after the report
	a.Declare(Component{Name: "backups", On: true, Run: func(context.Context) {}})

	if !strings.Contains(log.String(), "backups") {
		t.Errorf("a declaration after the process started must warn:\n%s", log.String())
	}
}

// A duplicate is not silently useless — it is silently doubled, including the
// background work, which is the part an operator cannot see in the report.
func TestADuplicateDeclarationWarns(t *testing.T) {
	var log strings.Builder
	a := newInternalApp(t)
	a.Log = slog.New(slog.NewTextHandler(&log, nil))

	a.Declare(Component{Name: "backups", On: true})
	a.Declare(Component{Name: "backups", On: true})

	if !strings.Contains(log.String(), "already declared") {
		t.Errorf("a duplicate name must warn:\n%s", log.String())
	}
}

// A component with no check is normal — most are informational.
func TestDeclareWithoutACheckIsFine(t *testing.T) {
	a := newInternalApp(t)
	a.Declare(Component{Name: "web push", On: true, Detail: "vapid key persisted"})

	if a.checks.Len() != 0 {
		t.Error("a component with no Ready must register no check")
	}
	if _, ok := declared(a, "web push"); !ok {
		t.Errorf("it must still appear in the report: %+v", a.Components())
	}
}
