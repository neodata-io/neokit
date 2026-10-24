package cache

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The regression this pins is silent and costly: a failed revalidation keeps
// serving the last good value — the right call for a tile — but GetOrFetch gave
// the caller no way to tell, so a scheduler integrating a cached power reading
// kept integrating a frozen one for the whole outage. Fetch says when the value
// is being kept alive.
func TestFetch_ReportsAKeptAliveValueAfterAFailedRevalidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := New()
		var down atomic.Bool
		fetches := 0
		fn := func(context.Context) (int, error) {
			fetches++
			if down.Load() {
				return 0, errors.New("upstream down")
			}
			return fetches, nil
		}

		v, st, err := Fetch(c, "k", time.Second, fn)
		require.NoError(t, err)
		assert.Equal(t, 1, v)
		assert.False(t, st.Failed)
		assert.True(t, st.FetchedAt.Equal(time.Now()), "a cold fetch is stamped now")
		fetchedAt := st.FetchedAt

		down.Store(true)
		time.Sleep(time.Second + time.Millisecond)  // past the TTL
		v, st, err = Fetch(c, "k", time.Second, fn) // stale read: served old value, refresh spawned
		require.NoError(t, err)
		assert.Equal(t, 1, v)
		assert.False(t, st.Failed, "the refresh has not failed yet when the stale read returns")
		synctest.Wait() // the background revalidation has failed by now

		v, st, err = Fetch(c, "k", time.Second, fn)
		require.NoError(t, err)
		assert.Equal(t, 1, v, "the last good value is kept alive")
		assert.True(t, st.Failed, "…and Status says it is")
		assert.True(t, st.FetchedAt.Equal(fetchedAt), "FetchedAt still names the good fetch")

		down.Store(false)
		time.Sleep(retryBackoff + time.Millisecond) // past the backoff: the next read refreshes
		_, _, err = Fetch(c, "k", time.Second, fn)
		require.NoError(t, err)
		synctest.Wait()
		v, st, err = Fetch(c, "k", time.Second, fn)
		require.NoError(t, err)
		assert.False(t, st.Failed, "a successful refresh clears the flag")
		assert.Equal(t, 3, v, "the cold fetch, the failed refresh, the good refresh")
		assert.True(t, st.FetchedAt.After(fetchedAt))
	})
}

func TestFetch_AColdFailureCarriesNoStatus(t *testing.T) {
	c := New()
	v, st, err := Fetch(c, "k", time.Second, func(context.Context) (string, error) {
		return "degraded view", errors.New("down")
	})
	require.Error(t, err)
	assert.Equal(t, "degraded view", v, "fn's value is still handed back")
	assert.Equal(t, Status{}, st, "nothing was cached, so there is nothing to report")
}

func TestGetOrFetch_IsFetchWithoutTheStatus(t *testing.T) {
	c := New()
	calls := 0
	fn := func(context.Context) (int, error) { calls++; return calls, nil }
	v, err := GetOrFetch(c, "k", time.Minute, fn)
	require.NoError(t, err)
	w, st, err := Fetch(c, "k", time.Minute, fn)
	require.NoError(t, err)
	assert.Equal(t, v, w, "same entry, same value")
	assert.Equal(t, 1, calls)
	assert.False(t, st.Failed)
}
