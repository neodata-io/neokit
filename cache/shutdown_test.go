package cache_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neodata-io/neokit/cache"
	"github.com/neodata-io/neokit/safe"

	"github.com/stretchr/testify/require"
)

// staleKey caches key with a TTL that has already run out, so the next read
// starts a background revalidation with fn.
func staleKey(t *testing.T, c *cache.Cache, key string) {
	t.Helper()
	_, err := cache.GetOrFetch(c, key, time.Millisecond, func(context.Context) (int, error) { return 1, nil })
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
}

// app.Run drains safe's group before closing what a fetch uses; a refresh
// outside that group could still be querying a closed database.
func TestABackgroundRefreshIsDrainedAtShutdown(t *testing.T) {
	c := cache.New()
	staleKey(t, c, "k")

	release := make(chan struct{})
	_, err := cache.GetOrFetch(c, "k", time.Minute, func(context.Context) (int, error) {
		<-release
		return 2, nil
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := safe.WaitGo(ctx); !errors.Is(err, safe.ErrDrainTimeout) {
		t.Errorf("WaitGo = %v while a refresh runs, want ErrDrainTimeout", err)
	}
	close(release)
	require.NoError(t, safe.WaitGo(t.Context()))
}

// A key can carry user input or a token; a panic log names the work, not the key.
func TestAPanickingRefreshDoesNotLogItsKey(t *testing.T) {
	var buf safeBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := cache.New()
	const key = "search?token=SECRET123"
	staleKey(t, c, key)
	_, err := cache.GetOrFetch(c, key, time.Minute, func(context.Context) (int, error) {
		panic("upstream exploded")
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return strings.Contains(buf.String(), "upstream exploded") },
		2*time.Second, 5*time.Millisecond, "the refresh panic was never logged")
	if strings.Contains(buf.String(), "SECRET123") {
		t.Errorf("panic log carries the cache key:\n%s", buf.String())
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
