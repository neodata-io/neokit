package safe

import (
	"log/slog"
	"sync"
	"time"
)

// goSafeWG tracks every live Go goroutine so shutdown can join them before
// closing the plugins and storage they poll. See [WaitGo].
var goSafeWG sync.WaitGroup

// goSafeRestartBackoff spaces out respawns so a goroutine that panics immediately
// and repeatedly can't hot-loop; it still recovers within seconds of a transient one.
const goSafeRestartBackoff = 5 * time.Second

// Go runs fn in a new goroutine and keeps it alive: a panic in a best-effort
// background job is logged and the goroutine is respawned after a short backoff,
// instead of crashing the whole process (which would drop every live SSE stream and
// the dashboard) or — the trap a plain one-shot recover falls into — silently dying
// so that one subsystem (peak tracking, energy accumulation, charge nudges) stops
// for the rest of the process lifetime with only a single log line. A clean return
// (fn finished, e.g. its ctx was cancelled on shutdown) ends the loop. These
// goroutines run outside Fiber's recover middleware, so they need their own guard.
func Go(name string, fn func()) {
	goSafeWG.Add(1)
	go func() {
		defer goSafeWG.Done()
		for {
			if !runGuarded(name, fn) {
				return // fn returned normally — nothing to respawn (shutdown)
			}
			slog.Warn("restarting panicked background goroutine", "goroutine", name)
			time.Sleep(goSafeRestartBackoff)
		}
	}()
}

// WaitGo blocks until every Go goroutine has returned — which happens
// once their context is cancelled at shutdown — or until timeout elapses,
// whichever comes first. Shutdown calls it before closing plugins and storage, so
// a background job mid-call (a scheduler polling a plugin) isn't cut off by having
// that plugin's connection closed underneath it. The bound guards against a job
// that ignores cancellation wedging the whole shutdown.
func WaitGo(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		goSafeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("timed out waiting for background goroutines to stop", "timeout", timeout)
	}
}

// runGuarded runs fn, recovering and logging any panic; it reports whether one
// occurred so Go knows to respawn (true) or stop (false, clean return).
func runGuarded(name string, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			LogPanic(name, r)
		}
	}()
	fn()
	return false
}
