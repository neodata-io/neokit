package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Reverse order is the entire reason this type exists: a step can only be torn
// down while everything depending on it is still alive.
func TestShutdownRunsStepsInReverseOrder(t *testing.T) {
	var order []string
	s := &Stack{Log: quiet()}
	for _, name := range []string{"store", "http", "metrics"} {
		s.Push(name, func(context.Context) error {
			order = append(order, name)
			return nil
		})
	}

	if err := s.Shutdown(context.Background(), time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	want := []string{"metrics", "http", "store"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// Steps run sequentially: concurrent teardown would discard the ordering that is
// the whole point.
func TestShutdownIsSequential(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0

	s := &Stack{Log: quiet()}
	for range 5 {
		s.Push("step", func(context.Context) error {
			mu.Lock()
			inFlight++
			maxInFlight = max(maxInFlight, inFlight)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		})
	}
	_ = s.Shutdown(context.Background(), time.Second)

	if maxInFlight != 1 {
		t.Errorf("max concurrent steps = %d, want 1", maxInFlight)
	}
}

// Every error is collected, not just the first: a caller needs to know
// everything that failed on the way down.
func TestShutdownJoinsEveryError(t *testing.T) {
	first := errors.New("http drain failed")
	second := errors.New("store close failed")

	s := &Stack{Log: quiet()}
	s.Push("store", func(context.Context) error { return second })
	s.Push("http", func(context.Context) error { return first })

	err := s.Shutdown(context.Background(), time.Second)
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Errorf("Shutdown err = %v, want both causes", err)
	}
	// Each is named so a log line says which step it was.
	if !strings.Contains(err.Error(), "http:") || !strings.Contains(err.Error(), "store:") {
		t.Errorf("err = %v, want each error prefixed with its step name", err)
	}
}

// A step that exceeds its budget must not stop the sweep: the alternative leaves
// a process holding a database handle because an HTTP server was slow to drain.
func TestASlowStepDoesNotStopTheRest(t *testing.T) {
	var ran []string
	s := &Stack{Log: quiet()}
	s.Push("store", func(context.Context) error { ran = append(ran, "store"); return nil })
	s.Push("slow", func(ctx context.Context) error {
		<-ctx.Done()
		ran = append(ran, "slow")
		return ctx.Err()
	})

	start := time.Now()
	err := s.Shutdown(context.Background(), 20*time.Millisecond)
	if took := time.Since(start); took > time.Second {
		t.Errorf("Shutdown took %v — the per-step budget was not applied", took)
	}
	if err == nil {
		t.Error("want the timed-out step's error surfaced")
	}
	if len(ran) != 2 || ran[1] != "store" {
		t.Errorf("ran = %v, want the slow step not to have blocked store", ran)
	}
}

// A panic in a teardown function would otherwise unwind out of Shutdown and skip
// every step below it — losing the database flush because the metrics server
// misbehaved.
func TestAPanickingStepDoesNotSkipTheStepsBelowIt(t *testing.T) {
	var reached bool
	s := &Stack{Log: quiet()}
	s.Push("store", func(context.Context) error { reached = true; return nil })
	s.Push("bad", func(context.Context) error { panic("boom") })

	err := s.Shutdown(context.Background(), time.Second)
	if !reached {
		t.Error("a panicking step skipped the steps below it")
	}
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("err = %v, want the panic surfaced as an error", err)
	}
}

// Shutdown is called from a signal handler and possibly again from a defer.
func TestShutdownIsIdempotent(t *testing.T) {
	var runs int
	s := &Stack{Log: quiet()}
	s.Push("once", func(context.Context) error { runs++; return nil })

	_ = s.Shutdown(context.Background(), time.Second)
	if err := s.Shutdown(context.Background(), time.Second); err != nil {
		t.Errorf("a second Shutdown must be a no-op, got %v", err)
	}
	if runs != 1 {
		t.Errorf("step ran %d times, want 1", runs)
	}
}

// A step registered after the sweep would never run; silently dropping it would
// leave a resource open with nothing to say so.
func TestPushAfterShutdownIsRefused(t *testing.T) {
	s := &Stack{Log: quiet()}
	_ = s.Shutdown(context.Background(), time.Second)
	s.Push("late", func(context.Context) error { t.Error("a late step must not run"); return nil })
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
	_ = s.Shutdown(context.Background(), time.Second)
}

// `stack.Push("store", store.Close)` on a store that turned out to be nil is a
// wiring mistake worth surviving — the alternative is a crash during the one
// code path that must not crash.
func TestNilStepIsIgnored(t *testing.T) {
	s := &Stack{Log: quiet()}
	s.Push("nil", nil)
	s.PushCloser("nil", nil)
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
	if err := s.Shutdown(context.Background(), time.Second); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// Names reports push order so a caller can assert the *real* teardown order
// instead of restating one — push order is a correctness property, and a step
// pushed in the wrong place closes a resource while something still depends on it.
func TestNamesReportsPushOrder(t *testing.T) {
	s := &Stack{Log: quiet()}
	for _, n := range []string{"database", "http", "metrics"} {
		s.Push(n, func(context.Context) error { return nil })
	}
	got := s.Names()
	want := []string{"database", "http", "metrics"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Must be a copy — a caller mutating it must not corrupt the stack's record.
	got[0] = "clobbered"
	if s.Names()[0] != "database" {
		t.Error("Names() handed out its backing array")
	}
	// Push ignores a nil step, so it must not appear here either.
	s.Push("ignored", nil)
	if len(s.Names()) != 3 {
		t.Errorf("Names() = %v, want the nil step absent", s.Names())
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestPushCloserAdaptsAnOrdinaryCloser(t *testing.T) {
	var closed bool
	s := &Stack{Log: quiet()}
	s.PushCloser("thing", closerFunc(func() error { closed = true; return nil }))

	if err := s.Shutdown(context.Background(), time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !closed {
		t.Error("PushCloser did not call Close")
	}
}

// A zero per-step budget means "only ctx bounds it", not "zero time".
func TestZeroBudgetMeansOnlyTheContextBounds(t *testing.T) {
	s := &Stack{Log: quiet()}
	var ran bool
	s.Push("slow-ish", func(ctx context.Context) error {
		time.Sleep(5 * time.Millisecond)
		ran = true
		return ctx.Err()
	})
	if err := s.Shutdown(context.Background(), 0); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !ran {
		t.Error("a zero budget must not cancel the step immediately")
	}
}

// Concurrent Push while another goroutine builds must be race-free under -race.
func TestPushIsRaceFree(t *testing.T) {
	s := &Stack{Log: quiet()}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Push("s", func(context.Context) error { return nil })
		}()
	}
	wg.Wait()
	if s.Len() != 16 {
		t.Errorf("Len = %d, want 16", s.Len())
	}
	_ = s.Shutdown(context.Background(), time.Second)
}

// A process that handles SIGINT but not SIGTERM shuts down cleanly on ^C and is
// killed uncleanly by every orchestrator — the default set is the value here.
func TestSignalsReturnsALiveContext(t *testing.T) {
	ctx, stop := Signals(context.Background())
	defer stop()
	select {
	case <-ctx.Done():
		t.Fatal("the context must not be cancelled before a signal arrives")
	default:
	}
	stop()
	<-ctx.Done() // stop cancels, restoring the default disposition
}

func BenchmarkShutdown(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		s := &Stack{Log: quiet()}
		for range 8 {
			s.Push("step", func(context.Context) error { return nil })
		}
		_ = s.Shutdown(context.Background(), time.Second)
	}
}
