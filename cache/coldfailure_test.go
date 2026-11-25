package cache_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neodata-io/neokit/cache"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queue starts n callers for key behind a fetch already in flight and returns
// once they have had time to reach it.
func queue(n int, call func()) *sync.WaitGroup {
	var started, done sync.WaitGroup
	for range n {
		started.Add(1)
		done.Go(func() {
			started.Done()
			call()
		})
	}
	started.Wait()
	time.Sleep(50 * time.Millisecond)
	return &done
}

// Callers queued behind a cold fetch used to re-run it one after another when it
// failed: twenty viewers of a dead upstream meant twenty timeouts end to end.
func TestGetOrFetch_QueuedCallersShareAColdFailure(t *testing.T) {
	c := cache.New()
	down := errors.New("upstream down")
	inFetch, release := make(chan struct{}, 32), make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) {
		calls.Add(1)
		inFetch <- struct{}{}
		<-release
		return -1, down // a degraded value handed back beside the error
	}
	call := func() {
		v, err := cache.GetOrFetch(c, "k", time.Minute, fetch)
		assert.ErrorIs(t, err, down)
		assert.Equal(t, -1, v, "the degraded value must reach every caller")
	}

	var leader sync.WaitGroup
	leader.Go(call)
	<-inFetch
	waiters := queue(20, call)
	close(release)
	leader.Wait()
	waiters.Wait()

	assert.Equal(t, int32(1), calls.Load(), "one failed fetch must answer everyone queued behind it")
}

// Sharing a result must not weaken Invalidate: a caller arriving after it has
// to see a fetch that started after it, not join one from before.
func TestGetOrFetch_ACallerAfterInvalidateDoesNotJoinAnOlderFetch(t *testing.T) {
	c := cache.New()
	inFetch, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) {
		n := calls.Add(1)
		if n == 1 { // the fetch that predates the mutation: hold it open
			close(inFetch)
			<-release
		}
		return int(n), nil
	}

	var before sync.WaitGroup
	before.Go(func() { _, _ = cache.GetOrFetch(c, "k", time.Minute, fetch) })
	<-inFetch

	c.Invalidate("k")
	after := make(chan int, 1)
	go func() {
		v, _ := cache.GetOrFetch(c, "k", time.Minute, fetch)
		after <- v
	}()
	time.Sleep(50 * time.Millisecond) // let it meet the older fetch, still in flight
	close(release)
	before.Wait()

	assert.Equal(t, 2, <-after, "the caller after Invalidate must get a fetch that started after it")
	v, err := cache.GetOrFetch(c, "k", time.Minute, fetch)
	require.NoError(t, err)
	assert.Equal(t, 2, v, "the older fetch must not overwrite the newer value")
}

// A panic unwinds only the caller that ran the fetch. It leaves no result to
// share, so those queued behind it must be released to fetch for themselves.
func TestGetOrFetch_APanickingColdFetchDoesNotStrandQueuedCallers(t *testing.T) {
	c := cache.New()
	inFetch, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			close(inFetch)
			<-release
			panic("upstream client bug")
		}
		return 7, nil
	}

	var leader sync.WaitGroup
	leader.Go(func() {
		defer func() { _ = recover() }()
		_, _ = cache.GetOrFetch(c, "k", time.Minute, fetch)
	})
	<-inFetch
	waiters := queue(5, func() {
		v, err := cache.GetOrFetch(c, "k", time.Minute, fetch)
		assert.NoError(t, err)
		assert.Equal(t, 7, v)
	})
	close(release)
	leader.Wait()
	waiters.Wait()

	assert.Equal(t, int32(2), calls.Load(), "one retry must serve everyone the panic released")
}

func TestInvalidate_OfAKeyThatIsNotCachedAllocatesNothing(t *testing.T) {
	c := cache.New()
	c.Invalidate("never-fetched")
	assert.Equal(t, 0, c.Len(), "invalidating an unknown key must not take a slot")
}

// Invalidate used to mint an entry per unknown key, so a burst of them pushed
// every live key out through the FIFO cap.
func TestInvalidate_OfUnknownKeysEvictsNothing(t *testing.T) {
	c := cache.New()
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) { return int(calls.Add(1)), nil }

	_, err := cache.GetOrFetch(c, "hot", time.Hour, fetch)
	require.NoError(t, err)
	for i := range 5000 { // more than the key cap
		c.Invalidate("ghost-" + strconv.Itoa(i))
	}
	_, err = cache.GetOrFetch(c, "hot", time.Hour, fetch)
	require.NoError(t, err)

	assert.Equal(t, int32(1), calls.Load(), "the live key must still be cached")
	assert.Equal(t, 1, c.Len())
}
