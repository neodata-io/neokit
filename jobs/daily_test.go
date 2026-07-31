package jobs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// brussels is a zone with a real DST transition, which is the whole reason this
// type exists.
func brussels(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Brussels")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	return loc
}

func TestNextIsAlwaysStrictlyInTheFuture(t *testing.T) {
	loc := brussels(t)
	d := Daily{Hour: 8}

	cases := map[string]time.Time{
		"before the hour": time.Date(2026, 3, 10, 7, 59, 0, 0, loc),
		"exactly on it":   time.Date(2026, 3, 10, 8, 0, 0, 0, loc),
		"after it":        time.Date(2026, 3, 10, 8, 0, 1, 0, loc),
		"late at night":   time.Date(2026, 3, 10, 23, 59, 0, 0, loc),
	}
	for name, from := range cases {
		t.Run(name, func(t *testing.T) {
			next := d.Next(from)
			if !next.After(from) {
				t.Errorf("Next(%v) = %v, must be strictly after", from, next)
			}
			if next.Hour() != 8 || next.Minute() != 0 {
				t.Errorf("Next(%v) = %v, want 08:00 local", from, next)
			}
		})
	}
	// "Exactly on it" must roll to tomorrow, or a job that fires precisely on the
	// hour would re-fire in a tight loop.
	from := time.Date(2026, 3, 10, 8, 0, 0, 0, loc)
	if got := d.Next(from); got.Day() != 11 {
		t.Errorf("Next at exactly the hour = %v, want the next day", got)
	}
}

// The reason Daily exists. A 24h interval walks an hour off at each transition
// and never walks back; advancing by a calendar day holds the local time.
func TestNextHoldsTheLocalHourAcrossDST(t *testing.T) {
	loc := brussels(t)

	cases := []struct {
		name string
		from time.Time
	}{
		// Spring forward: 2026-03-29 02:00 → 03:00 CET→CEST.
		{"the day before spring forward", time.Date(2026, 3, 28, 9, 0, 0, 0, loc)},
		// Fall back: 2026-10-25 03:00 → 02:00 CEST→CET.
		{"the day before fall back", time.Date(2026, 10, 24, 9, 0, 0, 0, loc)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Daily{Hour: 8}
			next := d.Next(tc.from)

			if next.Hour() != 8 {
				t.Errorf("Next(%v) = %v — the local hour drifted across the DST boundary", tc.from, next)
			}
			// And the naive alternative really would have been wrong, which is what
			// makes the calendar-day advance worth the code.
			if naive := tc.from.Add(24 * time.Hour); naive.Hour() == next.Hour() {
				t.Logf("note: +24h happens to agree here (%v)", naive)
			}
		})
	}
}

// Spring-forward at the exact missing hour: 02:30 does not exist on the
// transition day. Running late beats not running at all.
func TestNextSurvivesANonexistentLocalTime(t *testing.T) {
	loc := brussels(t)
	d := Daily{Hour: 2, Minute: 30}

	from := time.Date(2026, 3, 28, 12, 0, 0, 0, loc) // the day before
	next := d.Next(from)

	if !next.After(from) {
		t.Fatalf("Next(%v) = %v, must be in the future", from, next)
	}
	if next.Day() != 29 {
		t.Errorf("Next = %v, want the 29th", next)
	}
	// time.Date normalises the missing time forward rather than erroring.
	if next.Hour() != 3 && next.Hour() != 2 {
		t.Errorf("Next = %v, want it normalised into the surviving hour", next)
	}
}

// The scheduling arithmetic must be measured entirely on the injected clock. It
// previously mixed the two — Next() from Daily.Now, the wait from time.Until on
// the real wall clock — so an injected clock produced a delay with no relation to
// the schedule it was injected to control.
func TestDelayIsMeasuredOnTheInjectedClock(t *testing.T) {
	loc := brussels(t)
	// A frozen clock 30 minutes before the target, and deliberately far from the
	// real wall clock so a time.Until regression cannot pass by luck.
	frozen := time.Date(2020, 1, 2, 7, 30, 0, 0, loc)
	d := Daily{Hour: 8, Now: func() time.Time { return frozen }}

	if got := d.delay(frozen); got != 30*time.Minute {
		t.Errorf("delay = %v, want 30m measured on the injected clock", got)
	}
	// And it always agrees with Next, which is the property Run depends on.
	if got, want := d.delay(frozen), d.Next(frozen).Sub(frozen); got != want {
		t.Errorf("delay = %v, Next-derived = %v — the two must not drift", got, want)
	}
	if d.delay(frozen) <= 0 {
		t.Error("delay must be positive, or Run would spin")
	}
}

// Off by default: a process restarting five times in an afternoon must not send
// five morning digests.
func TestRunAtStartIsOffByDefault(t *testing.T) {
	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Daily{
		Name: "digest", Hour: 8, Log: quiet(),
		Do: func(context.Context) error { runs.Add(1); return nil },
	}.Run(ctx)

	time.Sleep(50 * time.Millisecond)
	if runs.Load() != 0 {
		t.Errorf("runs = %d, want 0 — a daily job must not fire on boot by default", runs.Load())
	}
}

func TestRunAtStartFiresImmediatelyWhenSet(t *testing.T) {
	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Daily{
		Name: "compact", Hour: 3, RunAtStart: true, Log: quiet(),
		Do: func(context.Context) error { runs.Add(1); return nil },
	}.Run(ctx)

	waitFor(t, func() bool { return runs.Load() == 1 }, "the start run")
}

func TestDailyReturnsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Daily{Name: "d", Hour: 8, Log: quiet(), Do: func(context.Context) error { return nil }}.Run(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return on cancellation — the timer is not being selected on")
	}
}

func TestDailyPanicsOnAnUnusableSchedule(t *testing.T) {
	cases := map[string]Daily{
		"hour too high":   {Name: "d", Hour: 24, Do: func(context.Context) error { return nil }},
		"hour negative":   {Name: "d", Hour: -1, Do: func(context.Context) error { return nil }},
		"minute too high": {Name: "d", Hour: 8, Minute: 60, Do: func(context.Context) error { return nil }},
		"no Do":           {Name: "d", Hour: 8},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Run must panic on an unusable schedule")
				}
			}()
			d.Run(context.Background())
		})
	}
}

func BenchmarkDailyNext(b *testing.B) {
	d := Daily{Hour: 8}
	from := time.Date(2026, 3, 28, 9, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	for b.Loop() {
		_ = d.Next(from)
	}
}
