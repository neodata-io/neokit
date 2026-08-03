package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/neodata-io/neokit/declare"
)

// ClosesOnShutdown registers close to run during teardown — after the HTTP
// drain, in reverse declaration order — and adds name to the boot report. For a
// dependency you built; neokit's own packages register themselves.
func (a *App) ClosesOnShutdown(name, detail string, close func(ctx context.Context) error) {
	if close == nil {
		panic("app: ClosesOnShutdown requires a close function")
	}
	declare.Add(a, name, declare.Detail(detail), declare.Close(close))
}

// ChecksReadiness registers ready as a /readyz check and adds name to the boot
// report. The check and the line travel together on purpose — a check that can
// fail readiness while appearing nowhere in the report is invisible.
func (a *App) ChecksReadiness(name, detail string, ready func(ctx context.Context) error) {
	if ready == nil {
		panic("app: ChecksReadiness requires a ready function")
	}
	declare.Add(a, name, declare.Detail(detail), declare.Ready(ready))
}

// Component is one optional part of the process: whether it is on, a line for
// the boot report, a readiness check when it has a dependency worth probing, and
// a teardown step when it holds something.
//
// One declaration produces all of them, so a component is named once and cannot
// appear in the report under one name and in /readyz or the shutdown log under
// another.
//
// Ready is ignored when On is false — an unconfigured optional feature must never
// make a container look unready. Close is not: a non-nil Close means something
// was allocated and would otherwise leak.
//
// An alias, so a package that must stay light — see [declare] — can accept one
// without importing app.
type Component = declare.Component

// Declare records a component for the boot report, for readiness when it is on
// and has a check, and for teardown when it has a Close.
//
// Call it during boot, before [App.Run], from one goroutine: the report renders
// at the top of Run and background work starts there too, so a later declaration
// reaches neither.
//
// An empty Name panics, for the reason [New] rejects an empty Options.Name: the
// name labels four separate outputs, and an anonymous one is untraceable in all
// four. A duplicate name and a late declaration are warned about rather than
// refused — a warning names the mistake where a refusal would only take down a
// process over a report line.
func (a *App) Declare(s Component) {
	if strings.TrimSpace(s.Name) == "" {
		panic("app: Component.Name is required")
	}
	// Late is worse than confusing: Run has already rendered the report and
	// iterated the components, so this one's Run never starts at all — silently,
	// which is the failure a warning here exists to name.
	if a.started.Load() {
		a.Log.Warn("component declared after the process started; it misses the boot report and its background work never runs",
			"component", s.Name)
	}
	for _, existing := range a.components {
		if existing.Name == s.Name {
			a.Log.Warn("component already declared; the report lists it twice and its readiness, teardown and background work each run twice",
				"component", s.Name)
			break
		}
	}

	a.components = append(a.components, s)

	if s.On && s.Ready != nil {
		a.checks.Register(s.Name, s.Ready)
	}
	// Pushed here rather than at Run, so it unwinds among the caller's own steps
	// in the order they were declared.
	if s.Close != nil {
		a.Shutdown.Push(s.Name, s.Close)
	}
}

// Components returns the declared components in declaration order — including
// neokit's own (tracing, metrics export), so this and the boot report never
// disagree about what the process is.
func (a *App) Components() []Component {
	out := make([]Component, len(a.components))
	copy(out, a.components)
	return out
}

// report renders the boot block: what this process actually is.
//
// It exists because the alternative — which is what every service does by
// default — is a handful of stray log lines scattered across the wiring, from
// which nobody can answer "is tracing on?" without reading the code.
func (a *App) report(addr string) string {
	subs := a.Components()
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
