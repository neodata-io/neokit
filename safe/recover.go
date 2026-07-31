// Package safe guards background work that runs outside a request's own
// recover middleware — schedulers, event hooks, detached goroutines — so a
// panic there is logged with a stack trace and contained, rather than crashing
// the whole process or silently ending that one goroutine forever.
//
// [Do] guards a single run and cannot be misused. [Recover] is the deferred
// form. [Group] (see supervise.go) keeps a goroutine alive across repeated
// panics and joins the set at shutdown.
package safe

import (
	"context"
	"log/slog"
	"runtime/debug"
)

// Do runs fn, recovering and logging any panic, and reports whether one
// occurred.
//
// Prefer it over [Recover], which depends on being invoked *directly* by a
// deferred call — a property the compiler will not check. The natural-looking
//
//	defer func() { safe.Recover("job"); cleanup() }()
//
// guards nothing: recover() returns nil unless called by the deferred function
// itself. Do has no such positioning to get wrong.
func Do(name string, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			logPanic(nil, name, r)
		}
	}()
	fn()
	return false
}

// Recover recovers a panic in a detached goroutine and logs it with a stack
// trace. It must be the deferred call itself:
//
//	go func() { defer safe.Recover("myJob"); doWork() }()
//
// Wrapping it in another closure — `defer func() { safe.Recover("x") }()` —
// does nothing at all, for the reason given on [Do]. Prefer [Do] unless you
// need the panic to keep unwinding a specific frame.
func Recover(name string) {
	if r := recover(); r != nil {
		logPanic(nil, name, r)
	}
}

// LogPanic logs an already-recovered panic value with a stack trace. Callers
// that need to *react* to a panic (e.g. respawn the goroutine) recover it
// themselves and pass the value here so the log format stays identical to
// [Recover]'s.
func LogPanic(name string, r any) { logPanic(nil, name, r) }

// logPanic is the single formatting point. A nil logger means slog.Default().
func logPanic(log *slog.Logger, name string, r any) {
	if log == nil {
		log = slog.Default()
	}
	// LogAttrs rather than the variadic form: passing these through ...any boxes
	// each onto the heap. There is no request context at a panic site, so
	// Background is the honest value rather than a nil the callee must repair.
	log.LogAttrs(context.Background(), slog.LevelError, "goroutine panic",
		slog.String("goroutine", name),
		slog.Any("panic", r),
		slog.String("stack", string(debug.Stack())),
	)
}
