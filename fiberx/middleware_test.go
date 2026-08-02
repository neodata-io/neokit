package fiberx

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// apiBase stands in for whatever route prefix a real caller mounts under; the
// tests below only care that a *template* is used as the attribute, not what
// that template's literal prefix happens to be.
const apiBase = "/api/v1"

// durationMetric is the instrument the cardinality tests read.
const durationMetric = "http.server.request.duration"

// installMeterReader makes a fresh MeterProvider the global one and returns the
// reader to collect from.
//
// Global because that is where MetricsAndLogger gets its instruments, and it
// reads them when the middleware is *built* — so every caller must install this
// before constructing the middleware under test, or the instruments bind to a
// provider this reader cannot see.
func installMeterReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
}

// observations returns how many requests the duration histogram recorded
// carrying every one of attrs, and how many data points matched.
//
// The point count is what makes "this attribute value never became a series"
// assertable: a value that was never recorded has no data point at all, where
// the old Prometheus assertion could only see a zero.
func observations(t *testing.T, reader *sdkmetric.ManualReader, attrs ...attribute.KeyValue) (count uint64, points int) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	for _, scope := range rm.ScopeMetrics {
		for _, mtr := range scope.Metrics {
			if mtr.Name != durationMetric {
				continue
			}
			hist, ok := mtr.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want a float64 histogram", mtr.Name, mtr.Data)
			}
			for _, dp := range hist.DataPoints {
				if hasAll(dp.Attributes, attrs) {
					count += dp.Count
					points++
				}
			}
		}
	}
	return count, points
}

func hasAll(set attribute.Set, want []attribute.KeyValue) bool {
	for _, kv := range want {
		got, ok := set.Value(kv.Key)
		if !ok || got != kv.Value {
			return false
		}
	}
	return true
}

func route(path string) attribute.KeyValue { return attribute.String("http.route", path) }

// The route attribute must carry the route *template*, not the raw URL and not
// "/".
//
// This is a regression test with a specific bug behind it: MetricsAndLogger used
// to read c.Route().Path *before* c.Next(), at which point the router hasn't
// matched and Fiber still reports the middleware's own catch-all Use route. Every
// request therefore recorded path="/", which silently made the metrics useless
// (one latency series for the whole API) and stopped skipLog from ever matching
// a quieted route, so those requests were logged around the clock.
//
// The attribute is also the reason to insist on the template: a time series is
// never evicted, so labelling with the raw path would let a port scan mint a
// permanent series per probed URL.
func TestMetricsAttributesUseTheRouteTemplate(t *testing.T) {
	reader := installMeterReader(t)

	app := fiber.New()
	app.Use(NewErrors(nil).MetricsAndLogger())
	app.Get(apiBase+"/invites/:id", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	const template = apiBase + "/invites/:id"

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

	got, points := observations(t, reader, route(template), attribute.Int("http.response.status_code", 200))
	if got != 2 {
		t.Errorf("http.route=%q recorded %d observations, want 2 — the attribute is not the route template", template, got)
	}
	if points != 1 {
		t.Errorf("http.route=%q spread over %d data points, want 1", template, points)
	}

	// The concrete paths must not have minted series of their own.
	for _, raw := range []string{apiBase + "/invites/abc123", apiBase + "/invites/def456"} {
		if _, points := observations(t, reader, route(raw)); points != 0 {
			t.Errorf("http.route=%q minted its own series (%d data points) — raw paths must never become attributes", raw, points)
		}
	}
}

// A request that matches no route at all must not label itself with the URL it
// tried: that path is attacker-controlled, and an unbounded attribute on a
// series that is never evicted is how a scanner turns the metrics pipeline into
// a memory leak. Fiber folds it onto the Use route ("/"), which is bounded —
// that is what we assert.
func TestUnroutedRequestsDoNotMintSeries(t *testing.T) {
	reader := installMeterReader(t)

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

	if _, points := observations(t, reader, route(scanned)); points != 0 {
		t.Errorf("a scanned path minted its own series (%d data points) — this is the unbounded-cardinality case", points)
	}
	if _, points := observations(t, reader, route("/")); points != 1 {
		t.Errorf("the unrouted request landed on %d data points for http.route=\"/\", want 1", points)
	}
}

// The method is the other half of the same cardinality problem: it is a token
// the client chooses, so an unrecognised one must fold onto _OTHER rather than
// becoming an attribute value of its own.
//
// It takes a custom RequestMethods to get one this far — Fiber answers 501
// before the middleware chain for anything outside its configured set, which is
// why the default nine are all mapped explicitly and this is the only way to
// exercise the fold. An application that adds a method is also the one that
// would otherwise ship the unbounded attribute.
func TestUnknownMethodsFoldOntoOther(t *testing.T) {
	reader := installMeterReader(t)

	const bogus = "BREW"
	app := fiber.New(fiber.Config{RequestMethods: append(fiber.DefaultMethods, bogus)})
	app.Use(NewErrors(nil).MetricsAndLogger())
	app.Add([]string{bogus}, "/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(bogus, "/ok", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 — the custom method never reached the handler", resp.StatusCode)
	}
	_ = resp.Body.Close()

	method := func(v string) attribute.KeyValue { return attribute.String("http.request.method", v) }
	if _, points := observations(t, reader, method(bogus)); points != 0 {
		t.Errorf("method %q became an attribute value of its own (%d data points)", bogus, points)
	}
	if got, _ := observations(t, reader, method("_OTHER")); got != 1 {
		t.Errorf("_OTHER recorded %d observations, want 1 — an unknown method was not folded", got)
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
	e := NewErrors(nil)
	e.QuietPath = func(path string) bool {
		return path == apiBase+"/health" || path == apiBase+"/monitor/services"
	}

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
			if got := e.skipLog(tc.path, tc.status); got != tc.want {
				t.Errorf("skipLog(%q, %d) = %v, want %v", tc.path, tc.status, got, tc.want)
			}
		})
	}
}

// With no QuietPath configured (the default a caller with no noisy routes of
// its own gets for free), only the structural cases are silenced.
func TestSkipLogWithNoQuietPathConfigured(t *testing.T) {
	e := NewErrors(nil)

	if e.skipLog(apiBase+"/health", 200) {
		t.Error("skipLog silenced a path with no QuietPath configured")
	}
	if !e.skipLog(apiBase+"/home", 304) {
		t.Error("skipLog must still silence 304s with no QuietPath configured")
	}
}

func TestMetricsAndLoggerUsesTheConfiguredLogger(t *testing.T) {
	var logs bytes.Buffer
	e := NewErrors(nil)
	e.Log = slog.New(slog.NewTextHandler(&logs, nil))
	app := fiber.New()
	app.Use(e.MetricsAndLogger())
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	resp, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/ok", nil))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if !bytes.Contains(logs.Bytes(), []byte("msg=http")) {
		t.Errorf("configured logger did not receive request summary: %s", logs.String())
	}
}
