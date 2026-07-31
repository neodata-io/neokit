package cache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two packages sharing a *Cache can collide in the key namespace — the key is
// just a string, and nothing in the type system stops it. That used to panic
// inside the entry's critical section, in a Lock/Unlock pair with no defer, so
// the unwind skipped the Unlock and left the entry's mutex held forever. These
// pin the survivable behaviour that replaced it.

func TestGetOrFetch_TypeMismatchReturnsAnError(t *testing.T) {
	t.Parallel()
	c := New()

	_, err := GetOrFetch(c, "k", time.Hour, func(context.Context) (int, error) { return 1, nil })
	require.NoError(t, err)

	_, err = GetOrFetch(c, "k", time.Hour, func(context.Context) (string, error) { return "s", nil })

	require.Error(t, err, "a type mismatch must be reported, not panicked")
	assert.ErrorIs(t, err, ErrTypeMismatch)
	// The message has to name the key and both types, or the collision is
	// undiagnosable in a codebase with one shared cache.
	assert.Contains(t, err.Error(), `"k"`)
	assert.Contains(t, err.Error(), "int")
	assert.Contains(t, err.Error(), "string")
}

func TestGetOrFetch_KeyStaysUsableAfterATypeMismatch(t *testing.T) {
	t.Parallel()
	c := New()

	_, err := GetOrFetch(c, "k", time.Hour, func(context.Context) (int, error) { return 1, nil })
	require.NoError(t, err)

	_, err = GetOrFetch(c, "k", time.Hour, func(context.Context) (string, error) { return "s", nil })
	require.Error(t, err)

	// The regression: the entry's mutex used to be left locked, so everything
	// below blocked forever. Run it behind a timeout so a reintroduced deadlock
	// fails the test instead of hanging the suite.
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := GetOrFetch(c, "k", time.Hour, func(context.Context) (int, error) { return 2, nil })
		assert.NoError(t, err)
		assert.Equal(t, 1, v, "the correctly-typed value is still cached")
		c.Invalidate("k")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the key deadlocked after a type mismatch — the entry lock was not released")
	}
}

func TestGetOrFetch_TypeMismatchDoesNotEvictTheGoodValue(t *testing.T) {
	t.Parallel()
	c := New()

	_, err := GetOrFetch(c, "k", time.Hour, func(context.Context) (int, error) { return 7, nil })
	require.NoError(t, err)

	_, err = GetOrFetch(c, "k", time.Hour, func(context.Context) (string, error) { return "s", nil })
	require.Error(t, err)

	// The mismatched caller must not get to overwrite or drop what is there:
	// whichever package owns the key legitimately keeps working.
	called := false
	v, err := GetOrFetch(c, "k", time.Hour, func(context.Context) (int, error) {
		called = true
		return -1, nil
	})
	require.NoError(t, err)
	assert.False(t, called, "the cached value must survive a mismatched read")
	assert.Equal(t, 7, v)
}
