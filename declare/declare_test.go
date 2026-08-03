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

// The zero value must be inert: a caller that fills only Name and On has no
// Ready and no Close, and neither may be called.
func TestZeroComponentHasNoFuncs(t *testing.T) {
	var c declare.Component
	if c.Ready != nil || c.Close != nil {
		t.Error("zero Component must have nil Ready and Close")
	}
	if c.On {
		t.Error("zero Component must be off")
	}
}
