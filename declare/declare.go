// Package declare holds the vocabulary a neokit component uses to register
// itself, and nothing else. It imports only context so that a light package —
// sqlitex, say — can take a Declarer without pulling in Fiber, OpenTelemetry and
// Prometheus along with app.
package declare

import "context"

// Component is one part of a service: whether it is on, a line for the boot
// report, an optional readiness check and an optional teardown step.
type Component struct {
	// Name is the label in the boot report, the readiness check's name and the
	// shutdown step's name. Required.
	Name string

	// On reports whether this component is configured and active.
	On bool

	// Detail is one line of context: a path, an issuer, a schedule, or the
	// reason it is off.
	Detail string

	// Ready, when set on an On component, registers a readiness check.
	Ready func(ctx context.Context) error

	// Close releases what this component holds. It runs even when On is false —
	// a non-nil Close means something was allocated and would otherwise leak.
	Close func(ctx context.Context) error
}

// Declarer is what a component registers itself with. *app.App satisfies it, so
// a constructor takes a Declarer rather than the app itself.
type Declarer interface{ Declare(Component) }

// Option configures one aspect of a Component. Applied in order; last wins.
type Option func(*Component)

// Detail sets the report line's context: a path, an issuer, a schedule.
func Detail(s string) Option { return func(c *Component) { c.Detail = s } }

// Ready registers fn as the component's readiness check.
func Ready(fn func(ctx context.Context) error) Option {
	return func(c *Component) { c.Ready = fn }
}

// Close registers fn to run during shutdown.
func Close(fn func(ctx context.Context) error) Option {
	return func(c *Component) { c.Close = fn }
}

// Disabled marks the component off; why is the report's explanation.
func Disabled(why string) Option {
	return func(c *Component) { c.On = false; c.Detail = why }
}

// Add declares a component on d — on by default, shaped by opts. This is the
// registration seam every neokit package funnels through; nothing outside this
// package builds a Component by hand.
func Add(d Declarer, name string, opts ...Option) {
	c := Component{Name: name, On: true}
	for _, o := range opts {
		o(&c)
	}
	d.Declare(c)
}
