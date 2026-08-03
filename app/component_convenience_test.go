package app_test

import (
	"context"
	"errors"
	"testing"
)

// findComponent lives in app_test.go.

// One call: the report line and the teardown step, named once.
func TestClosesOnShutdownClosesAndReports(t *testing.T) {
	a := newApp(t)
	closed := false
	a.ClosesOnShutdown("plugins", "3 loaded", func(context.Context) error { closed = true; return nil })

	c, ok := findComponent(a, "plugins")
	if !ok {
		t.Fatal("plugins missing from the report")
	}
	if !c.On || c.Detail != "3 loaded" {
		t.Errorf("component = %+v", c)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Error("the registered close never ran")
	}
}

// One call: the report line and the /readyz check, named once.
func TestChecksReadinessReports(t *testing.T) {
	a := newApp(t)
	a.ChecksReadiness("cache", "redis://localhost", func(context.Context) error { return errors.New("down") })

	c, ok := findComponent(a, "cache")
	if !ok || !c.On || c.Detail != "redis://localhost" {
		t.Fatalf("component = %+v ok=%v", c, ok)
	}
	if c.Ready == nil {
		t.Fatal("the check was not carried into the component")
	}
}

// A nil function would register nothing while looking wired — the silent bug
// class this API exists to remove — so it crashes at boot instead.
func TestNilFunctionsPanic(t *testing.T) {
	a := newApp(t)
	for name, f := range map[string]func(){
		"ClosesOnShutdown": func() { a.ClosesOnShutdown("x", "", nil) },
		"ChecksReadiness":  func() { a.ChecksReadiness("x", "", nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s(nil) must panic", name)
				}
			}()
			f()
		}()
	}
}
