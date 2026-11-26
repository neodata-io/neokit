package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/errs"
)

const pathSecret = "SECRETTOKEN123"

// loggedApp is an app whose every log line lands in the returned buffer.
func loggedApp(t *testing.T, base config.Base) (*app.App, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	base.LogLevel, base.LogFormat = "debug", "json"
	a, err := app.New(app.Options{
		Name: "testapp",
		Base: base,
		Log:  slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, &logged
}

// recordSpans installs a recording tracer provider for one test.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return rec
}

// A token in the URL (an invite, a reset link) is a credential, so a failed or
// panicking request logs the route template, never the path it was sent to.
func TestASecretInThePathNeverReachesTheLog(t *testing.T) {
	a, logged := loggedApp(t, config.Base{})
	a.API.Get("/invites/:token", func(fiber.Ctx) error { return errs.ErrNotFound })
	a.API.Get("/boom/:token", func(fiber.Ctx) error { panic("boom") })

	for _, path := range []string{"/invites/", "/boom/"} {
		resp := request(t, a, http.MethodGet, a.APIBase+path+pathSecret, nil)
		_ = resp.Body.Close()
	}

	if strings.Contains(logged.String(), pathSecret) {
		t.Errorf("the path secret reached the log:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "/invites/:token") {
		t.Errorf("the failure line does not name the route template:\n%s", logged.String())
	}
}

// A panic unwinds past the request logger's normal exit, but the request still
// happened: it gets its summary line with the 500 the client saw.
func TestAPanickingRequestStillGetsItsSummaryLine(t *testing.T) {
	a, logged := loggedApp(t, config.Base{})
	a.API.Get("/boom", func(fiber.Ctx) error { panic("boom") })

	resp := request(t, a, http.MethodGet, a.APIBase+"/boom", nil)
	_ = resp.Body.Close()

	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, `"msg":"http"`) && strings.Contains(line, `"status":500`) {
			return
		}
	}
	t.Errorf("no http summary line with status 500:\n%s", logged.String())
}

// The span must carry the status the client got. A returned error is rendered
// after the middleware unwinds, so reading the response there says 200.
func TestTheSpanCarriesTheStatusOfAReturnedError(t *testing.T) {
	rec := recordSpans(t)
	a := newApp(t)
	a.API.Get("/missing", func(fiber.Ctx) error { return errs.ErrNotFound })
	a.API.Get("/broken", func(fiber.Ctx) error { return errors.New("disk on fire") })

	for path, want := range map[string]struct {
		status string
		code   codes.Code
	}{
		"/missing": {"404", codes.Unset},
		"/broken":  {"500", codes.Error},
	} {
		before := len(rec.Ended())
		resp := request(t, a, http.MethodGet, a.APIBase+path, nil)
		_ = resp.Body.Close()
		spans := rec.Ended()[before:]
		if len(spans) != 1 {
			t.Fatalf("%s: %d spans, want 1", path, len(spans))
		}
		s := spans[0]
		var got string
		for _, kv := range s.Attributes() {
			if kv.Key == "http.response.status_code" {
				got = kv.Value.String()
			}
		}
		if got != want.status || s.Status().Code != want.code {
			t.Errorf("%s: span status_code=%q code=%v, want %s/%v", path, got, s.Status().Code, want.status, want.code)
		}
	}
}

// A client may name its request for correlation, but only with a short, plain
// id; anything else is replaced, so a header cannot pad every log line.
func TestOnlyAShortPlainRequestIDIsKept(t *testing.T) {
	a := newApp(t)
	a.API.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(http.StatusNoContent) })

	for _, tc := range []struct {
		sent string
		kept bool
	}{
		{"abc-123_DEF.9", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
		{strings.Repeat("a", 8000), false},
		{"has space", false},
		{`quote"id`, false},
	} {
		resp := request(t, a, http.MethodGet, a.APIBase+"/ok", map[string]string{"X-Request-Id": tc.sent})
		_ = resp.Body.Close()
		got := resp.Header.Get("X-Request-Id")
		if (got == tc.sent) != tc.kept {
			t.Errorf("sent %.20q…: echoed %.20q…, kept=%v want %v", tc.sent, got, got == tc.sent, tc.kept)
		}
		if got == "" || len(got) > 64 {
			t.Errorf("sent %.20q…: echoed id of length %d", tc.sent, len(got))
		}
	}
}

// Behind a public proxy and without a token, the scrape is not handed to the
// internet; a scraper that connects directly still gets it.
func TestMetricsWithoutATokenAreRefusedThroughATrustedProxy(t *testing.T) {
	// 0.0.0.0 is the peer address of Fiber's in-memory test connection.
	for _, tc := range []struct {
		name    string
		proxies []string
		want    int
	}{
		{"no proxy configured", nil, http.StatusOK},
		{"peer is not the proxy", []string{"10.0.0.0/8"}, http.StatusOK},
		{"peer is the proxy", []string{"0.0.0.0"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAppWith(t, app.Options{Base: config.Base{TrustedProxies: tc.proxies}})
			if status, _ := get(t, a, app.MetricsPath, ""); status != tc.want {
				t.Errorf("status = %d, want %d", status, tc.want)
			}
		})
	}
}

// With a token the proxy is irrelevant: the token is the gate.
func TestAMetricsTokenStillAdmitsAScrapeThroughAProxy(t *testing.T) {
	a := newAppWith(t, app.Options{Base: config.Base{TrustedProxies: []string{"0.0.0.0"}, MetricsToken: "s3cr3t"}})
	if status, _ := get(t, a, app.MetricsPath, "Bearer s3cr3t"); status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
}
