// Package safe guards background work that runs outside a request's own
// recover middleware — schedulers, event hooks, detached goroutines — so a
// panic there is logged with a stack trace and contained, rather than
// crashing the whole process or silently ending that one goroutine forever.
// Recover guards a single run; Go (see supervise.go) keeps a goroutine alive
// across repeated panics.
package safe

import (
	"log/slog"
	"runtime/debug"
)

// Recover recovers a panic in a detached goroutine and logs it with a stack
// trace, so a panic in best-effort background work (schedulers, event hooks) is
// contained and diagnosable instead of crashing the whole process. Such
// goroutines run outside Fiber's recover middleware, so they need their own guard.
// Use it as the first deferred call in the goroutine:
//
//	go func() { defer safe.Recover("myJob"); doWork() }()
func Recover(name string) {
	if r := recover(); r != nil {
		LogPanic(name, r)
	}
}

// LogPanic logs an already-recovered panic value with a stack trace. Callers that
// need to *react* to a panic (e.g. respawn the goroutine) recover it themselves and
// pass the value here so the log format stays identical to Recover's.
func LogPanic(name string, r any) {
	slog.Error("goroutine panic", "goroutine", name, "panic", r, "stack", string(debug.Stack()))
}
