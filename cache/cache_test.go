package cache_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/neodata-io/neokit/cache"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetOrFetch_CachesWithinTTL(t *testing.T) {
	c := cache.New()
	var calls atomic.Int32

	fetch := func(context.Context) (int, error) {
		calls.Add(1)
		return 42, nil
	}

	for range 5 {
		v, err := cache.GetOrFetch(c, "k", time.Minute, fetch)
		require.NoError(t, err)
		assert.Equal(t, 42, v)
	}
	assert.Equal(t, int32(1), calls.Load(), "fn should run once within the TTL")
}

// Invalidate is the one path that must still make the caller wait: it is used
// after a command mutates state, where serving the value from before the mutation
// would show the household the state it just changed away from.
func TestGetOrFetch_RefetchesSynchronouslyAfterInvalidate(t *testing.T) {
	c := cache.New()
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) {
		return int(calls.Add(1)), nil
	}

	v1, _ := cache.GetOrFetch(c, "k", time.Minute, fetch)
	c.Invalidate("k")
	v2, _ := cache.GetOrFetch(c, "k", time.Minute, fetch)

	assert.Equal(t, 1, v1)
	assert.Equal(t, 2, v2, "invalidate must force a synchronous refetch, not serve stale")
}

// The core of stale-while-revalidate: crossing a TTL boundary hands back the
// previous value at once and refreshes behind it. Before this, the unlucky caller
// that happened to arrive first paid the whole upstream cost — which is what made
// an off television stall every home screen for 5s every 3 minutes.
func TestGetOrFetch_ServesStaleThenRevalidates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cache.New()
		var calls atomic.Int32
		fetch := func(context.Context) (int, error) {
			return int(calls.Add(1)), nil
		}

		v1, _ := cache.GetOrFetch(c, "k", 10*time.Millisecond, fetch)
		require.Equal(t, 1, v1)
		time.Sleep(20 * time.Millisecond) // let it go stale

		v2, _ := cache.GetOrFetch(c, "k", time.Minute, fetch)
		assert.Equal(t, 1, v2, "a stale read must return the last good value immediately")

		synctest.Wait()
		v3, _ := cache.GetOrFetch(c, "k", time.Minute, fetch)
		assert.Equal(t, 2, v3, "the background revalidation should publish the fresh value")
	})
}

// The property that actually matters to the home screen: a stale read is fast
// even when the upstream behind it is slow. This is the regression guard — if
// revalidation ever moves back onto the request path, the read blocks and this
// fails.
func TestGetOrFetch_StaleReadDoesNotWaitOnASlowFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cache.New()
		const fetchCost = 300 * time.Millisecond
		var calls atomic.Int32

		fetch := func(context.Context) (int, error) {
			n := calls.Add(1)
			if n > 1 { // the refresh, not the initial load
				time.Sleep(fetchCost)
			}
			return int(n), nil
		}

		_, _ = cache.GetOrFetch(c, "k", 10*time.Millisecond, fetch) // warm
		time.Sleep(20 * time.Millisecond)                           // go stale

		// Fake time only advances while the read blocks, so any wait shows up here.
		start := time.Now()
		v, err := cache.GetOrFetch(c, "k", time.Minute, fetch)
		elapsed := time.Since(start)

		require.NoError(t, err)
		assert.Equal(t, 1, v, "should have been served the stale value")
		assert.Zero(t, elapsed, "a stale read waited on the revalidation")

		time.Sleep(fetchCost) // let the refresh finish so the bubble can exit
		synctest.Wait()
	})
}

// A burst of readers hitting a stale key must start exactly one refresh, or the
// cache would amplify load precisely when the upstream is already slow.
func TestGetOrFetch_StaleBurstStartsOneRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cache.New()
		var calls atomic.Int32
		fetch := func(context.Context) (int, error) {
			n := calls.Add(1)
			if n > 1 {
				time.Sleep(50 * time.Millisecond) // hold the refresh open so readers pile up
			}
			return int(n), nil
		}

		_, _ = cache.GetOrFetch(c, "k", 10*time.Millisecond, fetch)
		time.Sleep(20 * time.Millisecond)

		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() {
				_, _ = cache.GetOrFetch(c, "k", time.Minute, fetch)
			})
		}
		wg.Wait()

		time.Sleep(time.Second) // past the held refresh
		synctest.Wait()
		assert.Equal(t, int32(2), calls.Load(), "a stale burst must collapse to one refresh")
	})
}

// A failed revalidation must not blank the tile: the last good value keeps being
// served. Nor may it be retried on every single read, which would turn the cache
// into a load amplifier against an upstream that is already down.
func TestGetOrFetch_FailedRevalidationKeepsLastGoodAndBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cache.New()
		var calls atomic.Int32
		fetch := func(context.Context) (int, error) {
			if n := calls.Add(1); n == 1 {
				return 99, nil
			}
			return 0, assert.AnError
		}

		v1, _ := cache.GetOrFetch(c, "k", 10*time.Millisecond, fetch)
		require.Equal(t, 99, v1)
		time.Sleep(20 * time.Millisecond)

		_, _ = cache.GetOrFetch(c, "k", time.Minute, fetch) // triggers the failing refresh
		synctest.Wait()
		require.Equal(t, int32(2), calls.Load(), "refresh should have run")

		for range 20 {
			v, err := cache.GetOrFetch(c, "k", time.Minute, fetch)
			require.NoError(t, err, "a failed revalidation must not surface as a caller error")
			assert.Equal(t, 99, v, "the last good value must keep being served")
		}
		assert.Equal(t, int32(2), calls.Load(),
			"a failed refresh must back off, not re-dial on every read")
	})
}

// A mutation landing while a refresh is in flight must win. The refresh read the
// world *before* the mutation, so publishing its result would silently reinstate
// the state the caller invalidated to get rid of.
func TestGetOrFetch_InvalidateDiscardsAnInFlightRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cache.New()
		release := make(chan struct{})
		var calls atomic.Int32

		fetch := func(context.Context) (int, error) {
			n := calls.Add(1)
			if n == 2 { // the background refresh: hold it open
				<-release
			}
			return int(n), nil
		}

		_, _ = cache.GetOrFetch(c, "k", 10*time.Millisecond, fetch)
		time.Sleep(20 * time.Millisecond)
		_, _ = cache.GetOrFetch(c, "k", time.Minute, fetch) // starts the held refresh
		synctest.Wait()
		require.Equal(t, int32(2), calls.Load(), "refresh should have started")

		c.Invalidate("k") // the mutation lands mid-refresh
		close(release)    // now let the stale refresh finish and try to publish
		synctest.Wait()

		v, err := cache.GetOrFetch(c, "k", time.Minute, fetch)
		require.NoError(t, err)
		assert.Equal(t, 3, v, "the post-invalidate fetch must win over the refresh that predates it")
	})
}

func TestGetOrFetch_SingleFlight(t *testing.T) {
	c := cache.New()
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond) // hold so callers pile up
		return 7, nil
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			v, err := cache.GetOrFetch(c, "k", time.Minute, fetch)
			assert.NoError(t, err)
			assert.Equal(t, 7, v)
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load(), "concurrent misses should collapse to one fetch")
}

func TestGetOrFetch_DoesNotCacheErrors(t *testing.T) {
	c := cache.New()
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) {
		calls.Add(1)
		return 0, assert.AnError
	}

	_, err1 := cache.GetOrFetch(c, "k", time.Minute, fetch)
	_, err2 := cache.GetOrFetch(c, "k", time.Minute, fetch)
	require.Error(t, err1)
	require.Error(t, err2)
	assert.Equal(t, int32(2), calls.Load(), "errors must not be cached")
}

// fn must never receive a request-scoped context. A revalidation outlives the
// request that triggered it, so a cancelled caller context would abort the fetch
// and the cache would never repopulate — it would serve an ever-staler value and
// re-dial forever. This is the invariant the whole signature exists to enforce.
func TestGetOrFetch_FetchContextIsNotTheCallers(t *testing.T) {
	c := cache.New()

	// The context must be inspected *inside* fn: the cache releases it as soon as
	// the fetch returns, so reading it afterwards would only observe that cleanup.
	var (
		errDuringFetch error
		hasDeadline    bool
		deadline       time.Time
	)

	_, err := cache.GetOrFetch(c, "cold", time.Minute, func(ctx context.Context) (int, error) {
		errDuringFetch = ctx.Err()
		deadline, hasDeadline = ctx.Deadline()
		return 1, nil
	})
	require.NoError(t, err)

	assert.NoError(t, errDuringFetch, "the fetch context must be live while fn runs")
	assert.True(t, hasDeadline, "the fetch context must carry the cache's backstop deadline")
	assert.True(t, deadline.After(time.Now()), "the backstop deadline must be in the future")
}

// The same guarantee for the background path, which is where a request-scoped
// context would actually have bitten: the refresh outlives the request that
// triggered it, so an inherited context would already be cancelled by the time
// the fetch ran, and the cache would never repopulate.
func TestGetOrFetch_RevalidationContextIsLive(t *testing.T) {
	c := cache.New()
	seen := make(chan error, 1)
	var calls atomic.Int32

	fetch := func(ctx context.Context) (int, error) {
		if calls.Add(1) > 1 { // the background revalidation
			seen <- ctx.Err()
		}
		return 1, nil
	}

	_, _ = cache.GetOrFetch(c, "k", 10*time.Millisecond, fetch)
	time.Sleep(20 * time.Millisecond)
	_, _ = cache.GetOrFetch(c, "k", time.Minute, fetch) // serves stale, refreshes behind

	select {
	case err := <-seen:
		assert.NoError(t, err, "the revalidation context must be live, not a dead request's")
	case <-time.After(time.Second):
		t.Fatal("the background revalidation never ran")
	}
}
