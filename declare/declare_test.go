package declare_test

import (
	"context"
	"errors"
	"testing"

	"github.com/neodata-io/neokit/declare"
)

// recorder is the smallest possible Declarer. It is also the shape every feature
// package's tests use, so it doubles as the worked example.
type recorder struct{ got []declare.Component }

func (r *recorder) Declare(c declare.Component) { r.got = append(r.got, c) }

// The interface has to be satisfiable without importing app — that is the entire
// reason this package exists.
func TestDeclarerIsSatisfiableWithoutApp(t *testing.T) {
	var d declare.Declarer = &recorder{}

	want := errors.New("down")
	d.Declare(declare.Component{
		Name: "database", On: true, Detail: "./data/app.db",
		Ready: func(context.Context) error { return want },
	})

	r := d.(*recorder)
	if len(r.got) != 1 {
		t.Fatalf("declared %d components, want 1", len(r.got))
	}
	got := r.got[0]
	if got.Name != "database" || !got.On || got.Detail != "./data/app.db" {
		t.Errorf("Component = %+v", got)
	}
	if err := got.Ready(context.Background()); !errors.Is(err, want) {
		t.Errorf("Ready err = %v, want %v", err, want)
	}
}

// Add means on: the common case carries no boolean.
func TestAddDefaultsToOn(t *testing.T) {
	var r recorder
	declare.Add(&r, "backups", declare.Detail("daily, keep 7"))
	c := r.got[0]
	if !c.On || c.Name != "backups" || c.Detail != "daily, keep 7" {
		t.Errorf("Component = %+v", c)
	}
	if c.Ready != nil || c.Close != nil {
		t.Error("options not passed must stay nil")
	}
}

// Disabled is the ✗ line, and the reason is the payload.
func TestDisabledTurnsOffWithReason(t *testing.T) {
	var r recorder
	declare.Add(&r, "login", declare.Disabled("not configured"))
	c := r.got[0]
	if c.On || c.Detail != "not configured" {
		t.Errorf("Component = %+v", c)
	}
}

// Ready and Close carry through, and errors survive the trip.
func TestReadyAndCloseCarryThrough(t *testing.T) {
	var r recorder
	boom := errors.New("boom")
	declare.Add(&r, "cache",
		declare.Ready(func(context.Context) error { return boom }),
		declare.Close(func(context.Context) error { return nil }))
	c := r.got[0]
	if err := c.Ready(context.Background()); !errors.Is(err, boom) {
		t.Errorf("Ready err = %v", err)
	}
	if c.Close == nil || c.Close(context.Background()) != nil {
		t.Error("Close missing or failing")
	}
}

// Run is the fourth thing a component declares about itself, alongside its
// report line, its readiness check and its teardown step.
func TestRunCarriesThrough(t *testing.T) {
	var r recorder
	ran := make(chan struct{})
	declare.Add(&r, "backups", declare.Run(func(context.Context) { close(ran) }))

	c := r.got[0]
	if c.Run == nil {
		t.Fatal("declare.Run left Component.Run nil")
	}
	c.Run(context.Background())
	select {
	case <-ran:
	default:
		t.Error("Component.Run did not call the registered function")
	}
}

// The zero value must be inert: a caller that fills only Name and On has no
// Ready, Close or Run, and none of them may be called.
func TestZeroComponentHasNoFuncs(t *testing.T) {
	var c declare.Component
	if c.Ready != nil || c.Close != nil || c.Run != nil {
		t.Error("zero Component must have nil Ready, Close and Run")
	}
	if c.On {
		t.Error("zero Component must be off")
	}
}
