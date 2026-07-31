package safe

import (
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
				g.Go("worker", func() {})
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
				_ = g.Wait(time.Millisecond)
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
	g.Go("job", func() {
		<-release
		close(done)
	})

	close(release)
	require.NoError(t, g.Wait(5*time.Second), "the drain must complete")

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
	g.Go("stuck", func() { <-block })

	require.ErrorIs(t, g.Wait(50*time.Millisecond), ErrDrainTimeout)
}

// Wait is not terminal: a drained group stays usable. The previous design
// crashed instead, so nothing pinned this.
func TestGroup_IsReusableAfterADrain(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	g.Go("first", func() {})
	require.NoError(t, g.Wait(5*time.Second))

	done := make(chan struct{})
	g.Go("second", func() { close(done) })
	require.NoError(t, g.Wait(5*time.Second))

	select {
	case <-done:
	default:
		t.Error("a group must still supervise work after being drained")
	}
}

func TestGroup_WaitOnAnIdleGroupReturnsImmediately(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}
	require.NoError(t, g.Wait(time.Second))
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
	b.Go("b-long-lived", func() { <-blockB })
	a.Go("a-quick", func() {})

	assert.NoError(t, a.Wait(2*time.Second),
		"group A must drain without waiting on group B's goroutine")
}

func TestGroup_RestartsAPanickingGoroutine(t *testing.T) {
	t.Parallel()
	g := &Group{Log: quietLogger()}

	var mu sync.Mutex
	calls := 0
	done := make(chan struct{})
	g.Go("flaky", func() {
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
