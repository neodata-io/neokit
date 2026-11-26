package safe_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neodata-io/neokit/safe"
)

// An upstream with a rate limit wants a fan-out that never has more than a few
// calls in flight, whatever the number of items.
func TestAllLimitNeverRunsMoreThanTheLimitAtOnce(t *testing.T) {
	var now, peak atomic.Int32
	items := make([]int, 20)
	for i := range items {
		items[i] = i
	}
	results, ok := safe.AllLimit(t.Context(), items, 3, 0, "test", func(_ context.Context, i int) (int, error) {
		n := now.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		now.Add(-1)
		return i * 2, nil
	})
	if p := peak.Load(); p > 3 {
		t.Errorf("peak concurrency = %d, want at most 3", p)
	}
	for i := range items {
		if !ok[i] || results[i] != i*2 {
			t.Fatalf("item %d = (%d, %v), want (%d, true) in item order", i, results[i], ok[i], i*2)
		}
	}
}

// Items still waiting for a slot when ctx ends are not started.
func TestAllLimitDoesNotStartItemsAfterCtxEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var started atomic.Int32
	_, ok := safe.AllLimit(ctx, []int{1, 2, 3, 4}, 1, 0, "test", func(ctx context.Context, i int) (int, error) {
		started.Add(1)
		cancel()
		<-ctx.Done()
		return i, ctx.Err()
	})
	if n := started.Load(); n != 1 {
		t.Errorf("started %d items, want only the one that was running", n)
	}
	for i, v := range ok {
		if v {
			t.Errorf("ok[%d] = true, want false for an item that never answered", i)
		}
	}
}
