package safe

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// budget is the drain window a shutdown step hands to Wait.
func budget(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// The regression this pins is process-fatal. sync.WaitGroup requires that an
// Add taking the counter off zero happens-before any Wait; Go and WaitGo were
// independent entry points over one package-level WaitGroup, so a process that
// spawned background work while a shutdown drain was joining died on
// "sync: WaitGroup is reused before previous Wait has returned" — thrown from
// inside the goroutine WaitGo itself spawned, where no caller could recover it.
//
// Without the fix this test does not fail; it crashes the whole test binary.
func TestGroup_SpawnDuringDrainDoesNotPanic(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)

	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				g.Go(context.Background(), "worker", func() {})
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				// Built inline rather than through budget: this loop runs
				// hundreds of thousands of times, and a t.Cleanup per iteration
				// is its own leak.
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				_ = g.Wait(ctx)
				cancel()
			}
		}
	}()

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestGroup_WaitJoinsItsGoroutines(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	release := make(chan struct{})
	done := make(chan struct{})
	g.Go(context.Background(), "job", func() {
		<-release
		close(done)
	})

	close(release)
	require.NoError(t, g.Wait(budget(t, 5*time.Second)), "the drain must complete")

	select {
	case <-done:
	default:
		t.Error("Wait returned before the goroutine had finished")
	}
}

// A drain that times out must say so. Returning normally is what the previous
// version did: the caller then tore down the very resources the stragglers were
// still using, silently reintroducing the race the drain exists to prevent.
func TestGroup_WaitReportsATimeout(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	block := make(chan struct{})
	defer close(block)
	g.Go(context.Background(), "stuck", func() { <-block })

	require.ErrorIs(t, g.Wait(budget(t, 50*time.Millisecond)), ErrDrainTimeout)
}

// Wait is not terminal: a drained group stays usable. The previous design
// crashed instead, so nothing pinned this.
func TestGroup_IsReusableAfterADrain(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	g.Go(context.Background(), "first", func() {})
	require.NoError(t, g.Wait(budget(t, 5*time.Second)))

	done := make(chan struct{})
	g.Go(context.Background(), "second", func() { close(done) })
	require.NoError(t, g.Wait(budget(t, 5*time.Second)))

	select {
	case <-done:
	default:
		t.Error("a group must still supervise work after being drained")
	}
}

func TestGroup_WaitOnAnIdleGroupReturnsImmediately(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}
	require.NoError(t, g.Wait(budget(t, time.Second)))
	assert.Zero(t, g.Len())
}

// One Group per subsystem is the whole point of the type: a shutdown in one
// must not block on unrelated goroutines owned by another.
func TestGroup_IsIndependentOfOtherGroups(t *testing.T) {
	t.Parallel()
	a := &Group{Log: quietLogger()}
	b := &Group{Log: quietLogger()}

	blockB := make(chan struct{})
	defer close(blockB)
	b.Go(context.Background(), "b-long-lived", func() { <-blockB })
	a.Go(context.Background(), "a-quick", func() {})

	assert.NoError(t, a.Wait(budget(t, 2*time.Second)),
		"group A must drain without waiting on group B's goroutine")
}

// A finishing goroutine must not report a clean drain on behalf of one still
// live: leave decrements and then takes the lock, and a Go landing in that
// window puts the count back up. Without the re-check, app.Run's drain returns
// nil with a job still writing and the next step closes the store under it.
//
// Driven step by step rather than raced for — the window is a few instructions
// wide, and enter/wake are exactly what Go's goroutine wraps.
func TestGroup_WakeWithWorkStillLiveKeepsTheWaiterWaiting(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	g.enter()                    // A, about to finish
	require.Zero(t, g.n.Add(-1)) // A's decrement lands; it has not reached the lock
	g.enter()                    // B is spawned in the window
	g.idle = make(chan struct{}) // a Wait that observed B arms the channel
	idle := g.idle
	g.wake() // A finally reaches the lock

	select {
	case <-idle:
		t.Fatal("a finished goroutine released the waiter while another was still live")
	default:
	}

	// And the wake-up is only deferred, not lost: B's own leave delivers it.
	g.leave()
	select {
	case <-idle:
	default:
		t.Error("the last goroutine to leave must release the waiter")
	}
}

// Respawning into a shutdown cannot finish the work and does hold the drain
// open — a component whose Run panics on every call would otherwise keep the
// group's count above zero for as long as the process lives, making every
// shutdown time out.
func TestGroup_GoStopsRespawningOnceTheContextIsDone(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.Go(ctx, "always-panics", func() { panic("boom") })

	require.NoError(t, g.Wait(budget(t, 5*time.Second)),
		"a panicking goroutine must not outlive a cancelled context")
	assert.Zero(t, g.Len())
}

func TestGroup_RestartsAPanickingGoroutine(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	var mu sync.Mutex
	calls := 0
	done := make(chan struct{})
	g.Go(context.Background(), "flaky", func() {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			panic("boom")
		}
		close(done)
	})

	select {
	case <-done:
	case <-time.After(restartBackoff + 5*time.Second):
		t.Fatal("a panicking goroutine was not restarted")
	}
}

func TestDo_GuardsAPanicAndReportsIt(t *testing.T) {
	t.Parallel()
	prev := slog.Default()
	slog.SetDefault(quietLogger())
	t.Cleanup(func() { slog.SetDefault(prev) })

	assert.True(t, Do("job", func() { panic("boom") }), "a panic must be reported")
	assert.False(t, Do("job", func() {}), "a clean run must not report a panic")
}
