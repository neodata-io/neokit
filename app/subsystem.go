package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Subsystem is one optional part of the process: whether it is on, a line for
// the boot report, and — when it has a dependency worth probing — a readiness
// check.
//
// One declaration produces both outputs, so a subsystem is named once and cannot
// appear in the report under one name and in /readyz under another.
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
}

// Declare records a subsystem for the boot report and, when it is on and has a
// check, for readiness.
func (a *App) Declare(s Subsystem) {
	a.mu.Lock()
	a.subsystems = append(a.subsystems, s)
	a.mu.Unlock()

	if s.On && s.Ready != nil {
		a.Health.Register(s.Name, s.Ready)
	}
}

// declareBuiltin is Declare for a subsystem neokit itself owns (tracing,
// metrics export), kept out of Subsystems() so that method reflects only what
// the application declared — see the builtins field comment on App.
func (a *App) declareBuiltin(s Subsystem) {
	a.mu.Lock()
	a.builtins = append(a.builtins, s)
	a.mu.Unlock()

	if s.On && s.Ready != nil {
		a.Health.Register(s.Name, s.Ready)
	}
}

// Subsystems returns the application's declared subsystems in declaration
// order. It deliberately excludes neokit's own built-ins (tracing, metrics
// export) — see report, which includes both.
func (a *App) Subsystems() []Subsystem {
	a.mu.RLock()
	defer a.mu.RUnlock()
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
	a.mu.RLock()
	subs := make([]Subsystem, 0, len(a.builtins)+len(a.subsystems))
	subs = append(subs, a.builtins...)
	subs = append(subs, a.subsystems...)
	a.mu.RUnlock()
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
