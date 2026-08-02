package clock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/neodata-io/neokit/clock"
)

// The point of a Fake is that it does not move on its own — a test that reads
// it twice and gets two instants cannot assert on an exact time.
func TestFakeOnlyMovesWhenMoved(t *testing.T) {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	f := clock.NewFake(start)

	if !f.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", f.Now(), start)
	}
	time.Sleep(2 * time.Millisecond)
	if !f.Now().Equal(start) {
		t.Error("a fake clock must not advance on its own")
	}

	f.Advance(90 * time.Minute)
	if want := start.Add(90 * time.Minute); !f.Now().Equal(want) {
		t.Errorf("after Advance: %v, want %v", f.Now(), want)
	}

	// Backwards, which is what an NTP correction looks like to code under test.
	f.Advance(-2 * time.Hour)
	if want := start.Add(-30 * time.Minute); !f.Now().Equal(want) {
		t.Errorf("after negative Advance: %v, want %v", f.Now(), want)
	}

	f.Set(start)
	if !f.Now().Equal(start) {
		t.Errorf("after Set: %v, want %v", f.Now(), start)
	}
}

// Code under test commonly reads the clock from a goroutine while the test
// advances it; without locking that is a data race, caught only under -race.
func TestFakeIsSafeForConcurrentUse(t *testing.T) {
	f := clock.NewFake(time.Unix(0, 0))

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); f.Advance(time.Second) }()
		go func() { defer wg.Done(); _ = f.Now() }()
	}
	wg.Wait()

	if got := f.Now(); !got.Equal(time.Unix(8, 0)) {
		t.Errorf("Now() = %v, want 8s past the epoch — an Advance was lost", got)
	}
}

// RealClock has to actually read the wall clock; a zero value would be a silent
// disaster in production wiring.
func TestRealClockReadsTheWallClock(t *testing.T) {
	got := clock.RealClock{}.Now()
	if time.Since(got) > time.Minute || got.IsZero() {
		t.Errorf("Now() = %v, want roughly now", got)
	}
}
