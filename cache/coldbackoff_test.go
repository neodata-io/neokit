package cache_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/neodata-io/neokit/cache"
)

var errDown = errors.New("down")

// A source that is down before its first read has no last good value to serve.
// Asking it again on every read would make every caller wait out its timeout,
// so WithBackoff remembers the failure for the back-off a failed refresh gets.
func TestGetOrFetchWithBackoff_RemembersAColdFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cache.New()
		var calls atomic.Int32
		fetch := func(context.Context) (string, error) {
			calls.Add(1)
			return "degraded", errDown
		}

		for range 5 {
			v, err := cache.GetOrFetchWithBackoff(c, "k", time.Minute, fetch)
			if !errors.Is(err, errDown) || v != "degraded" {
				t.Fatalf("GetOrFetch = (%q, %v), want the remembered (degraded, down)", v, err)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("fetched %d times within the back-off, want 1", n)
		}
	})
}

// After the back-off one caller starts a single retry behind the remembered
// answer; once it succeeds, the real value is served.
func TestGetOrFetchWithBackoff_RetriesOnceBehindTheAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := cache.New()
		var calls atomic.Int32
		release := make(chan struct{})
		fetch := func(context.Context) (string, error) {
			if calls.Add(1) == 1 {
				return "degraded", errDown
			}
			<-release // hold the retry until every caller has been answered
			return "fresh", nil
		}
		_, _ = cache.GetOrFetchWithBackoff(c, "k", time.Minute, fetch)

		time.Sleep(10 * time.Second)
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				// Answered at once, from what was remembered.
				if _, err := cache.GetOrFetchWithBackoff(c, "k", time.Minute, fetch); !errors.Is(err, errDown) {
					t.Errorf("err = %v, want the remembered failure while the retry runs", err)
				}
			})
		}
		wg.Wait()
		close(release)
		synctest.Wait()
		if n := calls.Load(); n != 2 {
			t.Errorf("fetched %d times, want the first and one retry", n)
		}

		v, err := cache.GetOrFetchWithBackoff(c, "k", time.Minute, fetch)
		if err != nil || v != "fresh" {
			t.Errorf("after a good retry GetOrFetch = (%q, %v), want (fresh, nil)", v, err)
		}
	})
}

// Invalidate is how a caller says "ask again now"; it forgets a failure too.
func TestGetOrFetchWithBackoff_InvalidateForgetsTheFailure(t *testing.T) {
	c := cache.New()
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errDown
		}
		return 7, nil
	}
	_, _ = cache.GetOrFetchWithBackoff(c, "k", time.Minute, fetch)

	c.Invalidate("k")
	v, err := cache.GetOrFetchWithBackoff(c, "k", time.Minute, fetch)
	if err != nil || v != 7 {
		t.Errorf("GetOrFetch after Invalidate = (%d, %v), want a fresh fetch", v, err)
	}
}

// Plain GetOrFetch keeps asking on every read: a caller that wants the next
// request to retry at once is the default.
func TestGetOrFetch_DoesNotRememberAFailureLeftByWithBackoff(t *testing.T) {
	c := cache.New()
	var calls atomic.Int32
	fetch := func(context.Context) (int, error) {
		calls.Add(1)
		return 0, errDown
	}
	_, _ = cache.GetOrFetchWithBackoff(c, "k", time.Minute, fetch)
	_, _ = cache.GetOrFetch(c, "k", time.Minute, fetch)
	if n := calls.Load(); n != 2 {
		t.Errorf("fetched %d times, want GetOrFetch to ask again", n)
	}
}
