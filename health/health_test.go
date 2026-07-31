package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neodata-io/neokit/health"
)

// The whole reason liveness and readiness are separate endpoints. /healthz must
// answer "is this process alive" without touching a dependency — otherwise a
// database blip gets a perfectly healthy container killed and restarted, which
// cannot possibly fix the database.
func TestLiveHandlerTouchesNoDependency(t *testing.T) {
	var called atomic.Bool
	r := health.New()
	r.Register("database", func(context.Context) error {
		called.Store(true)
		return errors.New("down")
	})

	rec := httptest.NewRecorder()
	health.LiveHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — liveness must not depend on anything", rec.Code)
	}
	if called.Load() {
		t.Error("liveness ran a readiness check")
	}
}

func TestReadyHandlerReportsAFailingCheckByName(t *testing.T) {
	r := health.New()
	r.Register("database", func(context.Context) error { return nil })
	r.Register("cache", func(context.Context) error { return errors.New("connection refused") })

	rec := httptest.NewRecorder()
	r.ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	var got health.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not a Result: %v (%s)", err, rec.Body)
	}
	if got.Ready {
		t.Error("Ready must be false when a check failed")
	}
	// The name is the actionable part: "not ready" alone tells an operator nothing.
	if !strings.Contains(rec.Body.String(), "cache") {
		t.Errorf("body must name the failing check: %s", rec.Body)
	}
}

func TestReadyHandlerIs200WhenEverythingPasses(t *testing.T) {
	r := health.New()
	r.Register("database", func(context.Context) error { return nil })

	rec := httptest.NewRecorder()
	r.ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// An empty registry is ready. A service with no dependencies must not be
// permanently unready just because nobody registered anything.
func TestEmptyRegistryIsReady(t *testing.T) {
	if got := health.New().Check(context.Background()); !got.Ready {
		t.Error("a registry with no checks must report ready")
	}
}

// Checks run concurrently: a readiness probe has a deadline, and three
// sequential 2-second checks would blow it while three parallel ones would not.
func TestChecksRunConcurrently(t *testing.T) {
	r := health.New()
	const n = 4
	for i := range n {
		_ = i
		r.Register("slow", func(ctx context.Context) error {
			select {
			case <-time.After(80 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}

	start := time.Now()
	got := r.Check(context.Background())
	took := time.Since(start)

	if !got.Ready {
		t.Errorf("all checks pass, so Ready must be true: %+v", got)
	}
	if took > 300*time.Millisecond {
		t.Errorf("took %v for %d × 80ms checks — they ran sequentially", took, n)
	}
}

// A check that hangs must not hang the endpoint. An unbounded readiness probe is
// how a rolling deploy stalls.
func TestASlowCheckIsBounded(t *testing.T) {
	r := health.New()
	r.Timeout = 30 * time.Millisecond
	r.Register("wedged", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })

	start := time.Now()
	got := r.Check(context.Background())
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Check took %v — the timeout was not applied", took)
	}
	if got.Ready {
		t.Error("a wedged check must report not-ready")
	}
}

// Context-aware checks already return at the deadline. This covers the more
// dangerous case: a third-party callback that never observes its context must
// not leave readiness blocked forever.
func TestANonCooperativeCheckIsBounded(t *testing.T) {
	r := health.New()
	r.Timeout = 30 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r.Register("wedged", func(context.Context) error {
		<-release
		return nil
	})

	start := time.Now()
	got := r.Check(context.Background())
	if took := time.Since(start); took > time.Second {
		t.Errorf("Check took %v — a context-ignoring check held readiness open", took)
	}
	if got.Ready || len(got.Checks) != 1 || got.Checks[0].Err == "" {
		t.Errorf("result = %+v, want the timed-out check reported as failed", got)
	}
}

// Readiness is polled on a timer, so bounding one sweep is only half the job: if
// every sweep started a fresh call, a permanently wedged dependency would strand
// a goroutine — and everything its check closed over — every few seconds until
// the process died of it. Counting entries is the direct statement of the
// invariant: N sweeps against a wedged check enter it once.
func TestAWedgedCheckIsEnteredOnceNoMatterHowOftenReadinessIsPolled(t *testing.T) {
	r := health.New()
	r.Timeout = 20 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var entered atomic.Int64
	r.Register("wedged", func(context.Context) error {
		entered.Add(1)
		<-release
		return nil
	})

	const sweeps = 5
	var last health.Result
	for range sweeps {
		last = r.Check(context.Background())
	}

	if n := entered.Load(); n != 1 {
		t.Errorf("check entered %d times over %d sweeps, want 1 — every sweep stranded a goroutine", n, sweeps)
	}
	if last.Ready {
		t.Error("a wedged check must keep the instance out of rotation")
	}
	if got := last.Checks[0].Err; !strings.Contains(got, "still running") {
		t.Errorf("error = %q, want it to name the check as stuck rather than merely slow", got)
	}
}

// A panicking check is a bug in that check, not a reason to take the endpoint
// down — an unrecovered panic in an HTTP handler is a 500 with no body, at
// exactly the moment an operator is asking what is wrong.
func TestAPanickingCheckIsReportedNotPropagated(t *testing.T) {
	r := health.New()
	r.Register("bad", func(context.Context) error { panic("boom") })

	got := r.Check(context.Background())
	if got.Ready {
		t.Error("a panicking check must count as failed")
	}
	if len(got.Checks) != 1 || got.Checks[0].OK {
		t.Errorf("Checks = %+v, want the panic recorded as a failure", got.Checks)
	}
}

// Registering the same name twice is a wiring mistake; the later one must not
// silently replace the earlier and halve the coverage.
func TestDuplicateNamesAreBothReported(t *testing.T) {
	r := health.New()
	r.Register("db", func(context.Context) error { return nil })
	r.Register("db", func(context.Context) error { return errors.New("nope") })

	got := r.Check(context.Background())
	if len(got.Checks) != 2 {
		t.Errorf("got %d checks, want both retained: %+v", len(got.Checks), got.Checks)
	}
	if got.Ready {
		t.Error("the failing duplicate must still make the registry unready")
	}
}

// time.Duration marshals as raw nanoseconds, so a field named tookMs carrying a
// nanosecond count is wrong by six orders of magnitude — and reads correctly
// while being wrong, which is why it needs a test rather than a careful reader.
func TestReadyBodyReportsMilliseconds(t *testing.T) {
	r := health.New()
	r.Register("slow", func(context.Context) error {
		time.Sleep(30 * time.Millisecond)
		return nil
	})

	rec := httptest.NewRecorder()
	r.ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	var got health.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not a Result: %v (%s)", err, rec.Body)
	}
	if len(got.Checks) != 1 {
		t.Fatalf("Checks = %+v, want one", got.Checks)
	}
	// A 30ms check is tens of ms, not tens of millions. The upper bound is what
	// catches a nanosecond count; the lower bound catches a zeroed field.
	if ms := got.Checks[0].TookMs; ms < 1 || ms > 5000 {
		t.Errorf("TookMs = %d, want a plausible millisecond count", ms)
	}
}
