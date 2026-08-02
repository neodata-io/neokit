package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/neodata-io/neokit/config"
)

// The readiness registry is unexported — reachable only by declaring a
// subsystem — so what Declare feeds into it can only be asserted from inside the
// package. The coupling itself is the contract worth pinning: one Declare, both
// outputs, or a subsystem ends up in the report under one name and in /readyz
// under another.
func newInternalApp(t *testing.T) *App {
	t.Helper()

	a, err := New(Options{
		Name: "testapp", Version: "1.2.3",
		Base: config.Base{Port: 0, MetricsPort: 0, LogLevel: "error", LogFormat: "json"},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func declared(a *App, name string) (Subsystem, bool) {
	for _, s := range a.Subsystems() {
		if s.Name == name {
			return s, true
		}
	}
	return Subsystem{}, false
}

// One Declare feeds both the boot report and the readiness set, so a subsystem
// is named once.
func TestDeclareRegistersAReadinessCheck(t *testing.T) {
	a := newInternalApp(t)
	a.Declare(Subsystem{
		Name: "database", On: true, Detail: "./data/app.db",
		Ready: func(context.Context) error { return nil },
	})

	if a.health.Len() != 1 {
		t.Errorf("readiness has %d checks, want the declared one", a.health.Len())
	}
	got, ok := declared(a, "database")
	if !ok {
		t.Fatalf("database missing from Subsystems(): %+v", a.Subsystems())
	}
	if !got.On || got.Detail != "./data/app.db" {
		t.Errorf("Subsystem = %+v", got)
	}
}

// An unconfigured optional feature must never make a container look unready —
// that would pull a working instance out of rotation over a feature nobody
// asked for.
func TestAnOffSubsystemRegistersNoCheck(t *testing.T) {
	a := newInternalApp(t)
	a.Declare(Subsystem{
		Name: "login", On: false, Detail: "not configured",
		Ready: func(context.Context) error { return errors.New("never called") },
	})

	if a.health.Len() != 0 {
		t.Error("an off subsystem must contribute no readiness check")
	}
	if got := a.health.Check(context.Background()); !got.Ready {
		t.Error("an off subsystem must not make the app unready")
	}
}

// A subsystem with no check is normal — most are informational.
func TestDeclareWithoutACheckIsFine(t *testing.T) {
	a := newInternalApp(t)
	a.Declare(Subsystem{Name: "web push", On: true, Detail: "vapid key persisted"})

	if a.health.Len() != 0 {
		t.Error("a subsystem with no Ready must register no check")
	}
	if _, ok := declared(a, "web push"); !ok {
		t.Errorf("it must still appear in the report: %+v", a.Subsystems())
	}
}
