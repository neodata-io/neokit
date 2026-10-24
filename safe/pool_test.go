package safe

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPool_RunsEveryTaskAndWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &Pool{Log: quietLogger()}
		var ran atomic.Int32
		for range 3 {
			assert.True(t, p.Go(t.Context(), "task", func(context.Context) { ran.Add(1) }))
		}
		require.NoError(t, p.Wait(t.Context()))
		assert.Equal(t, int32(3), ran.Load())
	})
}

func TestPool_BoundsConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const limit = 4
		p := &Pool{Limit: limit, Log: quietLogger()}
		gate := make(chan struct{})
		var running atomic.Int32
		for i := range 3 * limit {
			p.Go(t.Context(), strconv.Itoa(i), func(context.Context) {
				running.Add(1)
				<-gate
				running.Add(-1)
			})
		}
		// Every goroutine is parked: the first batch on the gate, the rest on the
		// semaphore.
		synctest.Wait()
		assert.Equal(t, int32(limit), running.Load(), "tasks running at once")
		close(gate)
		require.NoError(t, p.Wait(t.Context()))
	})
}

func TestPool_RefusesTasksOnceDraining(t *testing.T) {
	p := &Pool{Log: quietLogger()}
	require.NoError(t, p.Wait(t.Context()))
	var ran atomic.Int32
	assert.False(t, p.Go(t.Context(), "late", func(context.Context) { ran.Add(1) }))
	require.NoError(t, p.Wait(t.Context()))
	assert.Equal(t, int32(0), ran.Load(), "a task after Wait must not run")
}

func TestPool_RefusesTasksOnACancelledContext(t *testing.T) {
	p := &Pool{Log: quietLogger()}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.False(t, p.Go(ctx, "doomed", func(context.Context) { t.Error("ran into a shutdown") }))
	require.NoError(t, p.Wait(t.Context()))
}

func TestPool_QueuedTaskIsDroppedWhenItsContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &Pool{Limit: 1, Log: quietLogger()}
		ctx, cancel := context.WithCancel(t.Context())
		gate := make(chan struct{})
		p.Go(ctx, "first", func(context.Context) { <-gate })
		synctest.Wait() // "first" holds the only slot before anything else is spawned
		var ran atomic.Bool
		require.True(t, p.Go(ctx, "queued", func(context.Context) { ran.Store(true) }))
		synctest.Wait() // "queued" is parked on the semaphore
		cancel()        // shutdown reaches it there
		synctest.Wait() // …and it has left the queue before the slot frees
		close(gate)
		require.NoError(t, p.Wait(t.Context()))
		assert.False(t, ran.Load(), "a task still queued at shutdown must not start")
	})
}

func TestPool_PanicEndsTheTaskAndNothingElse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &Pool{Limit: 1, Log: quietLogger()}
		var runs atomic.Int32
		p.Go(t.Context(), "boom", func(context.Context) {
			runs.Add(1)
			panic("once")
		})
		p.Go(t.Context(), "after", func(context.Context) { runs.Add(1) })
		require.NoError(t, p.Wait(t.Context()))
		assert.Equal(t, int32(2), runs.Load(), "the panicking task ran once and released its slot")
	})
}

func TestPool_WaitHonoursTheDrainBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &Pool{Log: quietLogger()}
		gate := make(chan struct{})
		p.Go(t.Context(), "slow", func(context.Context) { <-gate })

		err := p.Wait(budget(t, time.Second))
		require.ErrorIs(t, err, ErrDrainTimeout)
		require.ErrorIs(t, err, context.DeadlineExceeded)

		close(gate)
		require.NoError(t, p.Wait(t.Context()))
	})
}
