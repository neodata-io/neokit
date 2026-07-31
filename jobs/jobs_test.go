package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// quiet silences a job's own logging so a test's output stays readable.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestMain silences the process-wide logger. Several tests panic deliberately,
// and safe.Do reports a panic to slog.Default with a full stack trace — signal in
// production, pure noise in `go test` output.
func TestMain(m *testing.M) {
	slog.SetDefault(quiet())
	os.Exit(m.Run())
}

// A restart is exactly when the work is most overdue, and a plain ticker waits
// out a full interval first.
func TestRunAtStartRunsBeforeTheFirstTick(t *testing.T) {
	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		Job{
			Name: "t", Every: time.Hour, RunAtStart: true, Log: quiet(),
			Do: func(context.Context) error { runs.Add(1); return nil },
		}.Run(ctx)
	}()

	waitFor(t, func() bool { return runs.Load() == 1 }, "the start run")
	cancel()
	<-done
	if got := runs.Load(); got != 1 {
		t.Errorf("runs = %d, want exactly 1 — the hourly tick must not have fired", got)
	}
}

func TestWithoutRunAtStartNothingRunsImmediately(t *testing.T) {
	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Job{
		Name: "t", Every: time.Hour, Log: quiet(),
		Do: func(context.Context) error { runs.Add(1); return nil },
	}.Run(ctx)

	time.Sleep(50 * time.Millisecond)
	if got := runs.Load(); got != 0 {
		t.Errorf("runs = %d, want 0 without RunAtStart", got)
	}
}

func TestTicksRepeatedlyUntilCancelled(t *testing.T) {
	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		Job{
			Name: "t", Every: time.Millisecond, Log: quiet(),
			Do: func(context.Context) error { runs.Add(1); return nil },
		}.Run(ctx)
	}()

	waitFor(t, func() bool { return runs.Load() >= 3 }, "three ticks")
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return on cancellation")
	}
}

// The timeout is the whole reason this package exists: the ctx a job is handed
// lives for the process, so an unbounded call that never returns does not lose
// one sample — the tick never completes, the loop never reaches the next fire,
// and the job is silently dead for the rest of the process's life.
func TestTimeoutBoundsOneRunWithoutEndingTheLoop(t *testing.T) {
	var runs atomic.Int64
	var deadlines atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Job{
		Name: "t", Every: 5 * time.Millisecond, Timeout: 10 * time.Millisecond, Log: quiet(),
		Do: func(ctx context.Context) error {
			runs.Add(1)
			<-ctx.Done() // a call that would otherwise never return
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				deadlines.Add(1)
			}
			return ctx.Err()
		},
		OnError: func(context.Context, error) {},
	}.Run(ctx)

	waitFor(t, func() bool { return runs.Load() >= 2 }, "a second tick after the first timed out")
	if deadlines.Load() == 0 {
		t.Error("the run was not bounded by Timeout")
	}
}

// A panicking Do must not stop the schedule. Recovering inside the tick — rather
// than letting it unwind to the supervisor — also preserves the loop's own
// ticker phase.
func TestPanicInDoDoesNotEndTheLoop(t *testing.T) {
	var runs atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Job{
		Name: "t", Every: time.Millisecond, Log: quiet(),
		Do: func(context.Context) error {
			if runs.Add(1) == 1 {
				panic("boom")
			}
			return nil
		},
	}.Run(ctx)

	waitFor(t, func() bool { return runs.Load() >= 3 }, "ticks after the panicking one")
}

func TestOnErrorReceivesTheFailure(t *testing.T) {
	want := errors.New("upstream down")
	got := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go Job{
		Name: "t", Every: time.Hour, RunAtStart: true, Log: quiet(),
		Do:      func(context.Context) error { return want },
		OnError: func(_ context.Context, err error) { got <- err },
	}.Run(ctx)

	select {
	case err := <-got:
		if !errors.Is(err, want) {
			t.Errorf("OnError got %v, want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("OnError was never called")
	}
}

// A non-positive interval is a programming error no runtime behaviour can paper
// over: treating it as "run once" turns a periodic job into a one-shot with
// nothing to notice it.
func TestRunPanicsOnAnUnusableJob(t *testing.T) {
	cases := map[string]Job{
		"zero interval":     {Name: "t", Every: 0, Do: func(context.Context) error { return nil }},
		"negative interval": {Name: "t", Every: -time.Second, Do: func(context.Context) error { return nil }},
		"no Do":             {Name: "t", Every: time.Second},
	}
	for name, j := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Run must panic on an unusable job")
				}
			}()
			j.Run(context.Background())
		})
	}
}

// A cancelled context must stop the loop promptly even mid-run.
func TestCancellationStopsAJobAlreadyRunning(t *testing.T) {
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		Job{
			Name: "t", Every: time.Millisecond, RunAtStart: true, Log: quiet(),
			Do: func(ctx context.Context) error {
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			},
			OnError: func(context.Context, error) {},
		}.Run(ctx)
	}()

	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancellation reached Do")
	}
}

// Concurrent jobs sharing nothing must be race-free under -race.
func TestConcurrentJobsAreRaceFree(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var runs atomic.Int64

	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Job{
				Name: "j", Every: time.Millisecond, RunAtStart: true, Log: quiet(),
				Do: func(context.Context) error { runs.Add(1); return nil },
			}.Run(ctx)
		}()
		_ = i
	}
	waitFor(t, func() bool { return runs.Load() >= 16 }, "runs across all jobs")
	cancel()
	wg.Wait()
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ── Benchmarks ──────────────────────────────────────────────────────────────

// The per-tick overhead a caller pays over calling Do directly.
func BenchmarkTickBounded(b *testing.B) {
	j := Job{Name: "b", Every: time.Hour, Timeout: time.Second, Log: quiet(),
		Do: func(context.Context) error { return nil }}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		j.tick(ctx)
	}
}

func BenchmarkTickUnbounded(b *testing.B) {
	j := Job{Name: "b", Every: time.Hour, Log: quiet(),
		Do: func(context.Context) error { return nil }}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		j.tick(ctx)
	}
}
