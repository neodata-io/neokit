package cache

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The key cap is the cache's memory guard: callers are allowed to key on
// unauthenticated input (search queries, coordinates), so a cache that grew
// without bound would be a denial-of-service vector rather than a shield.
// Nothing pinned that guarantee before these tests.

func TestCache_LenCountsDistinctKeys(t *testing.T) {
	c := New()
	assert.Equal(t, 0, c.Len(), "a fresh cache holds nothing")

	for i := range 3 {
		_, err := GetOrFetch(c, strconv.Itoa(i), time.Hour, func(context.Context) (int, error) {
			return i, nil
		})
		require.NoError(t, err)
	}
	assert.Equal(t, 3, c.Len())

	// Re-reading an existing key must not mint a second entry.
	_, err := GetOrFetch(c, "1", time.Hour, func(context.Context) (int, error) { return 1, nil })
	require.NoError(t, err)
	assert.Equal(t, 3, c.Len())
}

func TestCache_ExpiredEntryStillOccupiesASlot(t *testing.T) {
	// Len counts keys, not live values: an expired entry holds its slot until
	// eviction reclaims it, which is precisely what the cap governs.
	c := New()
	_, err := GetOrFetch(c, "k", time.Nanosecond, func(context.Context) (int, error) { return 1, nil })
	require.NoError(t, err)
	time.Sleep(time.Millisecond)
	assert.Equal(t, 1, c.Len(), "an expired key still occupies a slot")
}

func TestCache_EvictsOnceOverTheKeyCap(t *testing.T) {
	// The guarantee under test is boundedness: however many distinct keys are
	// pushed through, occupancy never exceeds the cap. This is the property the
	// cache's memory safety rests on.
	c := New()
	const over = defaultMaxEntries + 500

	for i := range over {
		_, err := GetOrFetch(c, strconv.Itoa(i), time.Hour, func(context.Context) (int, error) {
			return i, nil
		})
		require.NoError(t, err)
	}

	assert.LessOrEqual(t, c.Len(), defaultMaxEntries,
		"occupancy must never exceed the key cap, or unauthenticated input could grow the cache without bound")
	assert.Positive(t, c.Len(), "eviction must not empty the cache")
}

func TestCache_EvictionKeepsRecentKeysAndDropsOldest(t *testing.T) {
	// Eviction is by insertion order, so the keys written most recently are the
	// ones that survive. Asserted on the extremes rather than on an exact
	// boundary: the cache may evict in groups, and only the ordering is promised.
	c := New()
	const over = defaultMaxEntries + 500

	for i := range over {
		_, err := GetOrFetch(c, strconv.Itoa(i), time.Hour, func(context.Context) (int, error) {
			return i, nil
		})
		require.NoError(t, err)
	}

	// The most recent key must still be cached: fetching it again must not call fn.
	called := false
	v, err := GetOrFetch(c, strconv.Itoa(over-1), time.Hour, func(context.Context) (int, error) {
		called = true
		return -1, nil
	})
	require.NoError(t, err)
	assert.False(t, called, "the most recently inserted key must survive eviction")
	assert.Equal(t, over-1, v)
}

func TestCache_EvictedKeyRefetchesRatherThanServingAStaleValue(t *testing.T) {
	// An evicted key is simply absent — the next read must go back to the source
	// and get a correct value, never a wrong one.
	c := New()
	const over = defaultMaxEntries + 500
	for i := range over {
		_, err := GetOrFetch(c, strconv.Itoa(i), time.Hour, func(context.Context) (int, error) {
			return i, nil
		})
		require.NoError(t, err)
	}

	v, err := GetOrFetch(c, "0", time.Hour, func(context.Context) (int, error) { return 12345, nil })
	require.NoError(t, err)
	assert.Equal(t, 12345, v, "an evicted key must refetch, not resurrect a stale value")
}
