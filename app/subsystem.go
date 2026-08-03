package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Subsystem is one optional part of the process: whether it is on, a line for
// the boot report, a readiness check when it has a dependency worth probing, and
// a teardown step when it holds something.
//
// One declaration produces all of them, so a subsystem is named once and cannot
// appear in the report under one name and in /readyz or the shutdown log under
// another.
type Subsystem struct {
	// Name is the label in the boot report and the readiness check's name.
	Name string

	// On reports whether this subsystem is configured and active.
	On bool

	// Detail is one line of context: a path, an issuer, a schedule, or the
	// reason it is off ("OTEL_EXPORTER_OTLP_ENDPOINT unset").
	Detail string

	// Ready, when set on an On subsystem, registers a /readyz check.
	//
	// It is ignored when On is false: an unconfigured optional feature must never
	// make a container look unready, which would take a working instance out of
	// rotation for a feature nobody asked for.
	Ready func(ctx context.Context) error

	// Close releases what this subsystem holds; [App.Declare] pushes it onto
	// [App.Shutdown]. Unlike Ready it runs even when On is false — a non-nil
	// Close means something was allocated and would otherwise leak.
	Close func(ctx context.Context) error
}

// Declare records a subsystem for the boot report, for readiness when it is on
// and has a check, and for teardown when it has a Close.
//
// Call it during boot, before [App.Run], from one goroutine: the report renders
// at the top of Run, so a later declaration would miss it anyway.
//
// An empty Name panics, for the reason [New] rejects an empty Options.Name: the
// name labels three separate outputs, and an anonymous one is untraceable in all
// three. A duplicate name and a late declaration are warned about rather than
// refused — both still produce a working process, just a confusing one.
func (a *App) Declare(s Subsystem) {
	if strings.TrimSpace(s.Name) == "" {
		panic("app: Subsystem.Name is required")
	}
	if a.booted.Load() {
		a.Log.Warn("subsystem declared after the boot report; it will not appear there",
			"subsystem", s.Name)
	}
	for _, existing := range a.subsystems {
		if existing.Name == s.Name {
			a.Log.Warn("subsystem already declared; the report, readiness and teardown will each list it twice",
				"subsystem", s.Name)
			break
		}
	}

	a.subsystems = append(a.subsystems, s)

	if s.On && s.Ready != nil {
		a.health.Register(s.Name, s.Ready)
	}
	// Pushed here rather than at Run, so it unwinds among the caller's own steps
	// in the order they were declared.
	if s.Close != nil {
		a.Shutdown.Push(s.Name, s.Close)
	}
}

// Subsystems returns the declared subsystems in declaration order — including
// neokit's own (tracing, metrics export), so this and the boot report never
// disagree about what the process is.
func (a *App) Subsystems() []Subsystem {
	out := make([]Subsystem, len(a.subsystems))
	copy(out, a.subsystems)
	return out
}

// report renders the boot block: what this process actually is.
//
// It exists because the alternative — which is what every service does by
// default — is a handful of stray log lines scattered across the wiring, from
// which nobody can answer "is tracing on?" without reading the code.
func (a *App) report(addr string) string {
	subs := a.Subsystems()
	// Longest name sets the column, so the details line up and the block can be
	// scanned rather than read.
	width := 0
	for _, s := range subs {
		if len(s.Name) > width {
			width = len(s.Name)
		}
	}
	// On first, then off, each alphabetical: the interesting half is at the top.
	sort.SliceStable(subs, func(i, j int) bool {
		if subs[i].On != subs[j].On {
			return subs[i].On
		}
		return subs[i].Name < subs[j].Name
	})

	var b strings.Builder
	head := a.Name
	if a.Version != "" {
		head += " " + a.Version
	}
	fmt.Fprintf(&b, "%s · %s\n", head, addr)
	for _, s := range subs {
		mark := "✗"
		if s.On {
			mark = "✓"
		}
		fmt.Fprintf(&b, "  %s %-*s  %s\n", mark, width, s.Name, s.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}
