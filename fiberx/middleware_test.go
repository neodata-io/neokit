package fiberx

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// apiBase stands in for whatever route prefix a real caller mounts under; the
// tests below only care that a *template* is used as the label, not what that
// template's literal prefix happens to be.
const apiBase = "/api/v1"

// The `path` label must carry the route *template*, not the raw URL and not "/".
//
// This is a regression test with a specific bug behind it: MetricsAndLogger used
// to read c.Route().Path *before* c.Next(), at which point the router hasn't
// matched and Fiber still reports the middleware's own catch-all Use route. Every
// request therefore recorded path="/", which silently made the metrics useless
// (one latency series for the whole API) and stopped skipLog from ever matching
// a quieted route, so those requests were logged around the clock.
//
// The label is also the reason to insist on the template: a Prometheus series is
// never evicted, so labelling with the raw path would let a port scan mint a
// permanent series per probed URL.
func TestMetricsLabelsUseTheRouteTemplate(t *testing.T) {
	app := fiber.New()
	app.Use(NewErrors(nil).MetricsAndLogger())
	app.Get(apiBase+"/invites/:id", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	const template = apiBase + "/invites/:id"

	before := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET", template, "200"))

	// Two *different* concrete IDs must fold into the one template series.
	for _, id := range []string{"abc123", "def456"} {
		resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, apiBase+"/invites/"+id, nil))
		if err != nil {
			t.Fatalf("request %s: %v", id, err)
		}
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("request %s: status = %d, want 200", id, resp.StatusCode)
		}
	}

	if got := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET", template, "200")) - before; got != 2 {
		t.Errorf("counter for path=%q rose by %v, want 2 — the label is not the route template", template, got)
	}

	// The concrete paths must not have minted series of their own.
	for _, raw := range []string{apiBase + "/invites/abc123", apiBase + "/invites/def456"} {
		if n := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET", raw, "200")); n != 0 {
			t.Errorf("path=%q minted its own series (%v) — raw paths must never become labels", raw, n)
		}
	}
}

// A request that matches no route at all must not label itself with the URL it
// tried: that path is attacker-controlled, and an unbounded label on a
// never-evicted series is how a scanner turns /metrics into a memory leak. Fiber
// folds it onto the Use route ("/"), which is bounded — that is what we assert.
func TestUnroutedRequestsDoNotMintSeries(t *testing.T) {
	app := fiber.New()
	app.Use(NewErrors(nil).MetricsAndLogger())
	app.Get(apiBase+"/health", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	const scanned = "/wp-admin/setup-config.php"

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, scanned, nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	if n := testutil.ToFloat64(httpRequestsTotal.WithLabelValues("GET", scanned, "404")); n != 0 {
		t.Errorf("a scanned path minted its own series (%v) — this is the unbounded-cardinality case", n)
	}
}

// skipLog only ever gets to do its job if `path` is the template — with the old
// pre-Next() read it received "/" for every request and a quieted route never
// matched, which is why a caller's health probes could end up logged forever.
//
// QuietPath is the seam a caller uses to name its own noisy routes (this
// package cannot know them); the structural cases (304, 101) are silenced
// regardless of QuietPath.
func TestSkipLogSilencesQuietedPaths(t *testing.T) {
	prev := QuietPath
	QuietPath = func(path string) bool {
		return path == apiBase+"/health" || path == apiBase+"/monitor/services"
	}
	t.Cleanup(func() { QuietPath = prev })

	cases := []struct {
		name   string
		path   string
		status int
		want   bool
	}{
		{"successful health probe", apiBase + "/health", 200, true},
		{"service health sweep", apiBase + "/monitor/services", 200, true},
		{"not modified", apiBase + "/home", 304, true},
		{"sse upgrade", apiBase + "/invite/x/events", 101, true},
		{"a failing health probe still speaks up", apiBase + "/health", 503, false},
		{"ordinary traffic is logged", apiBase + "/invites/:id", 200, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := skipLog(tc.path, tc.status); got != tc.want {
				t.Errorf("skipLog(%q, %d) = %v, want %v", tc.path, tc.status, got, tc.want)
			}
		})
	}
}

// With no QuietPath configured (the default a caller with no noisy routes of
// its own gets for free), only the structural cases are silenced.
func TestSkipLogWithNoQuietPathConfigured(t *testing.T) {
	prev := QuietPath
	QuietPath = nil
	t.Cleanup(func() { QuietPath = prev })

	if skipLog(apiBase+"/health", 200) {
		t.Error("skipLog silenced a path with no QuietPath configured")
	}
	if !skipLog(apiBase+"/home", 304) {
		t.Error("skipLog must still silence 304s with no QuietPath configured")
	}
}
