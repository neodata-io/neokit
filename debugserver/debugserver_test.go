package debugserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// get exercises the built server's handler directly, so no port is bound.
func get(t *testing.T, srv *http.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestMetricsIsAlwaysMounted(t *testing.T) {
	rec := get(t, New(Config{Addr: ":0"}), "/metrics")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text exposition format", ct)
	}
}

// The whole point of the Pprof knob: a consumer who does not ask for it must not
// get a heap-dump endpoint. Importing net/http/pprof registers those handlers on
// http.DefaultServeMux as a side effect, so "it is not mounted here" is a claim
// worth a test rather than a reading of the code.
func TestPprofIsAbsentUnlessRequested(t *testing.T) {
	srv := New(Config{Addr: ":0"})

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/heap"} {
		if code := get(t, srv, path).Code; code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 — pprof leaked onto a server that did not enable it", path, code)
		}
	}
}

func TestPprofMountsIndexAndSubpathsWhenRequested(t *testing.T) {
	srv := New(Config{Addr: ":0", Pprof: true})

	// pprof.Index serves both its own listing and the profile subpaths that have
	// no dedicated handler (/heap, /goroutine), so one registration covers both —
	// which is exactly the thing a hand-rolled copy tends to get wrong.
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/symbol"} {
		if code := get(t, srv, path).Code; code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, code)
		}
	}
}

// /metrics must stay reachable with pprof on: they share one mux, and a pattern
// clash would silently shadow one of them.
func TestPprofDoesNotDisplaceMetrics(t *testing.T) {
	if code := get(t, New(Config{Addr: ":0", Pprof: true}), "/metrics").Code; code != http.StatusOK {
		t.Errorf("GET /metrics = %d with pprof enabled, want 200", code)
	}
}

func TestGathererOverrideIsServed(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "neokit_debugserver_probe_total",
		Help: "A metric registered nowhere near the default registry.",
	}))

	body := get(t, New(Config{Addr: ":0", Gatherer: reg}), "/metrics").Body.String()
	if !strings.Contains(body, "neokit_debugserver_probe_total") {
		t.Errorf("the configured Gatherer was not used; body was:\n%s", body)
	}
}

// A zero ReadHeaderTimeout lets a client that opens a connection and never
// finishes its headers pin it forever. The default exists so that omitting the
// field cannot produce that server.
func TestReadHeaderTimeoutIsNeverZero(t *testing.T) {
	if got := New(Config{Addr: ":0"}).ReadHeaderTimeout; got <= 0 {
		t.Errorf("ReadHeaderTimeout = %v, want a positive default", got)
	}
	if got := New(Config{Addr: ":0", ReadHeaderTimeout: time.Second}).ReadHeaderTimeout; got != time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want the configured 1s", got)
	}
}

func TestAddrIsCarriedThrough(t *testing.T) {
	if got := New(Config{Addr: "127.0.0.1:9101"}).Addr; got != "127.0.0.1:9101" {
		t.Errorf("Addr = %q, want 127.0.0.1:9101", got)
	}
}

// Serve exists for this one line: ListenAndServe reports the caller's own
// Shutdown as ErrServerClosed, and a caller that routes its return value to a
// fatal-error channel turns every clean exit into a crash.
//
// No synchronisation is needed before Shutdown: ListenAndServe on an
// already-shut-down server returns ErrServerClosed too, so either interleaving
// exercises the same mapping.
func TestServeReturnsNilAfterShutdown(t *testing.T) {
	srv := New(Config{Addr: "127.0.0.1:0"})

	errc := make(chan error, 1)
	go func() { errc <- Serve(srv) }()

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Serve returned %v after a graceful Shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}

// A listen failure must still reach the caller — that is the difference between
// "shut down cleanly" and "never started at all", and collapsing the two is how
// a bound port becomes a silent no-op.
func TestServeReportsARealListenFailure(t *testing.T) {
	if err := Serve(New(Config{Addr: "127.0.0.1:99999"})); err == nil {
		t.Fatal("Serve returned nil for an unbindable address, want the listen error")
	}
}
