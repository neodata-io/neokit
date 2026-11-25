package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neodata-io/neokit/httpc"
)

// capture records what a sender actually put on the wire.
type capture struct {
	method string
	path   string
	header http.Header
	body   string
}

// recorder serves a test backend and captures the request.
func recorder(t *testing.T, status int) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.path, got.header, got.body = r.Method, r.URL.Path, r.Header.Clone(), string(b)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestWebhookPostsTheDocumentedPayload(t *testing.T) {
	srv, got := recorder(t, http.StatusOK)
	s := NewWebhook(srv.URL, "sh4red", Options{})

	err := s.Send(t.Context(), Notification{
		Title: "Backup complete", Body: "3 files", Level: LevelSuccess,
		URL: "https://app.test/backups", Tags: []string{"backup"},
		Data: map[string]any{"files": 3},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if secret := got.header.Get("X-Webhook-Secret"); secret != "sh4red" {
		t.Errorf("X-Webhook-Secret = %q", secret)
	}

	var p WebhookPayload
	if err := json.Unmarshal([]byte(got.body), &p); err != nil {
		t.Fatalf("body is not the documented payload: %v (%s)", err, got.body)
	}
	if p.Title != "Backup complete" || p.Level != LevelSuccess || p.URL != "https://app.test/backups" {
		t.Errorf("payload = %+v", p)
	}
	if p.Timestamp.IsZero() {
		t.Error("payload must carry a timestamp")
	}
}

// No secret configured means no header at all, rather than an empty one a
// receiver might compare against.
func TestWebhookOmitsTheSecretHeaderWhenUnset(t *testing.T) {
	srv, got := recorder(t, http.StatusOK)
	if err := NewWebhook(srv.URL, "", Options{}).Send(t.Context(), Notification{Title: "t"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, present := got.header["X-Webhook-Secret"]; present {
		t.Error("an unset secret must not send the header")
	}
}

func TestNtfySendsTitleAndBodySeparately(t *testing.T) {
	srv, got := recorder(t, http.StatusOK)
	s := NewNtfy(srv.URL+"/my-topic", "t0ken", Options{})

	err := s.Send(t.Context(), Notification{
		Title: "Disk almost full", Body: "92% used", Level: LevelWarning,
		URL: "https://app.test/storage",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got.body != "92% used" {
		t.Errorf("body = %q, want the raw message", got.body)
	}
	if title := got.header.Get("Title"); title != "Disk almost full" {
		t.Errorf("Title = %q", title)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer t0ken" {
		t.Errorf("Authorization = %q", auth)
	}
	// Warning and failure are raised above default so they reach a phone in
	// do-not-disturb.
	if p := got.header.Get("Priority"); p != "4" {
		t.Errorf("Priority = %q, want 4 for a warning", p)
	}
	if click := got.header.Get("Click"); click != "https://app.test/storage" {
		t.Errorf("Click = %q", click)
	}
}

// A newline in a header lets the value inject additional headers, and ntfy
// rejects a malformed one outright.
func TestNtfyStripsControlCharactersFromHeaders(t *testing.T) {
	srv, got := recorder(t, http.StatusOK)
	s := NewNtfy(srv.URL, "", Options{})

	err := s.Send(t.Context(), Notification{
		Title: "line one\r\nX-Injected: yes",
		Tags:  []string{"tag\none", "a,b"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.ContainsAny(got.header.Get("Title"), "\r\n") {
		t.Errorf("Title kept a control character: %q", got.header.Get("Title"))
	}
	if _, injected := got.header["X-Injected"]; injected {
		t.Error("a header was injected through the title")
	}
	// A comma separates tags on the wire, so one inside a tag would split it.
	if tags := got.header.Get("Tags"); strings.Count(tags, ",") != 1 {
		t.Errorf("Tags = %q, want exactly one separator", tags)
	}
}

// A caller that supplies no tags should still get a useful icon.
func TestNtfyFallsBackToALevelTag(t *testing.T) {
	for level, want := range map[Level]string{
		LevelSuccess: "white_check_mark",
		LevelWarning: "warning",
		LevelFailure: "rotating_light",
		LevelInfo:    "bell",
	} {
		srv, got := recorder(t, http.StatusOK)
		if err := NewNtfy(srv.URL, "", Options{}).Send(t.Context(),
			Notification{Title: "t", Level: level}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if tags := got.header.Get("Tags"); tags != want {
			t.Errorf("level %s → Tags %q, want %q", level, tags, want)
		}
	}
}

func TestAppriseUsesItsOwnTypeVocabulary(t *testing.T) {
	srv, got := recorder(t, http.StatusOK)
	s := NewApprise(srv.URL+"/notify/", Options{})

	if err := s.Send(t.Context(), Notification{
		Title: "Build failed", Body: "step 3", Level: LevelFailure, Tags: []string{"ci"},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var p struct{ Title, Body, Type, Tag string }
	if err := json.Unmarshal([]byte(got.body), &p); err != nil {
		t.Fatalf("body: %v (%s)", err, got.body)
	}
	// Apprise's vocabulary is info/success/warning/failure, which is why Level
	// uses those names rather than a parallel set needing translation.
	if p.Type != "failure" || p.Title != "Build failed" || p.Tag != "ci" {
		t.Errorf("payload = %+v", p)
	}
}

// An unrecognised level is treated as info rather than rejected: a notification
// is best-effort, and dropping one over a typo'd severity is the wrong trade.
func TestAnUnknownLevelDegradesToInfo(t *testing.T) {
	srv, got := recorder(t, http.StatusOK)
	if err := NewApprise(srv.URL, Options{}).Send(t.Context(),
		Notification{Title: "t", Level: Level("shouty")}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(got.body, `"type":"info"`) {
		t.Errorf("body = %s, want an info fallback", got.body)
	}
}

// The status must classify through httpc.Classify like every other upstream
// error, so a caller can tell "your token is wrong" from "the service is down".
func TestAFailedDeliveryClassifiesAsAnAPIError(t *testing.T) {
	cases := map[int]httpc.Fault{
		http.StatusUnauthorized:       httpc.FaultAuth,
		http.StatusServiceUnavailable: httpc.FaultUnavailable,
	}
	for status, wantFault := range cases {
		srv, _ := recorder(t, status)
		err := NewWebhook(srv.URL, "", Options{}).Send(t.Context(), Notification{Title: "t"})
		if err == nil {
			t.Fatalf("status %d: want an error", status)
		}
		if _, ok := errors.AsType[*httpc.APIError](err); !ok {
			t.Errorf("status %d: err = %v, want an *httpc.APIError", status, err)
			continue
		}
		if got := httpc.Classify(err); got != wantFault {
			t.Errorf("status %d: Classify = %v, want %v", status, got, wantFault)
		}
	}
}

// One dead backend must not suppress the others — the shape a caller otherwise
// writes by hand with an early return that silently drops the rest.
func TestMultiSendsToEveryBackendAndJoinsErrors(t *testing.T) {
	ok, okGot := recorder(t, http.StatusOK)
	bad, badGot := recorder(t, http.StatusInternalServerError)

	m := Multi{
		NewWebhook(ok.URL, "", Options{}),
		NewApprise(bad.URL, Options{}),
		nil, // a nil sender is skipped rather than panicking
	}
	err := m.Send(t.Context(), Notification{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("want the failing backend's error")
	}
	if !strings.Contains(err.Error(), "apprise") {
		t.Errorf("err = %v, want the failing backend named", err)
	}
	if okGot.body == "" || badGot.body == "" {
		t.Error("every backend must be attempted, not just up to the first failure")
	}
}

func TestMultiWithNoFailuresReturnsNil(t *testing.T) {
	ok, _ := recorder(t, http.StatusOK)
	m := Multi{NewWebhook(ok.URL, "", Options{})}
	if err := m.Send(t.Context(), Notification{Title: "t"}); err != nil {
		t.Errorf("Send: %v", err)
	}
}

// A notification URL routinely carries its credential in the query string; a
// transport failure renders the whole URL.
func TestATransportFailureDoesNotLeakTheURLCredential(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	err := NewWebhook(url+"/hook?token=s3cret-token", "", Options{}).
		Send(t.Context(), Notification{Title: "t"})
	if err == nil {
		t.Fatal("want a transport error")
	}
	if strings.Contains(err.Error(), "s3cret-token") {
		t.Errorf("the error leaked the URL credential: %v", err)
	}
}

// The path can be the whole credential — an Apprise key, an ntfy topic — so an
// error names the target by host and nothing else.
func TestAnErrorNamesTheTargetByHostOnly(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := dead.URL
	dead.Close()
	host := strings.TrimPrefix(base, "http://")

	senders := map[string]Sender{
		"apprise key":   NewApprise(base+"/notify/S3CRET-PATH", Options{}),
		"ntfy topic":    NewNtfy(base+"/S3CRET-PATH", "", Options{}),
		"malformed URL": NewWebhook("http://hooks.test/%zz/S3CRET-PATH", "", Options{}),
	}
	for name, s := range senders {
		err := s.Send(t.Context(), Notification{Title: "t"})
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if strings.Contains(err.Error(), "S3CRET-PATH") {
			t.Errorf("%s: the error leaked the URL path: %v", name, err)
		}
		if name != "malformed URL" && !strings.Contains(err.Error(), host) {
			t.Errorf("%s: err = %v, want it to name the host %s", name, err, host)
		}
	}
}

// A redirected POST either arrives as a body-less GET that "succeeds" having
// delivered nothing, or replays the secret and payload to wherever it points.
func TestARedirectIsReportedNotFollowed(t *testing.T) {
	clients := map[string]*http.Client{"default client": nil, "caller's client": {}}
	for name, hc := range clients {
		for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			var hits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/elsewhere", status)
			}))

			err := NewWebhook(srv.URL, "sh4red", Options{HTTPClient: hc}).Send(t.Context(), Notification{Title: "t", Body: "b"})
			srv.Close()
			target.Close()

			apiErr, ok := errors.AsType[*httpc.APIError](err)
			if !ok || apiErr.StatusCode != status {
				t.Errorf("%s, %d: err = %v, want the redirect reported as an APIError", name, status, err)
			}
			if hits.Load() != 0 {
				t.Errorf("%s, %d: the redirect target was contacted", name, status)
			}
		}
	}
}

// "webhook API 400" with no reason sends an operator to the backend's own logs.
func TestARejectionCarriesTheBackendsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("topic name is not allowed"))
	}))
	defer srv.Close()

	err := NewNtfy(srv.URL, "", Options{}).Send(t.Context(), Notification{Title: "t"})
	apiErr, ok := errors.AsType[*httpc.APIError](err)
	if !ok {
		t.Fatalf("err = %v, want an *httpc.APIError", err)
	}
	if apiErr.Body != "topic name is not allowed" {
		t.Errorf("APIError.Body = %q, want the backend's reason", apiErr.Body)
	}
}

// A backend that never stops writing must not hold a send for its whole timeout.
func TestAnEndlessResponseBodyIsNotDrainedForever(t *testing.T) {
	chunk := strings.Repeat("x", 32<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for r.Context().Err() == nil {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	start := time.Now()
	if err := NewWebhook(srv.URL, "", Options{Timeout: 3 * time.Second}).Send(t.Context(), Notification{Title: "t"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Send took %v: the response body was drained until the timeout", elapsed)
	}
}

// Pins what the package doc promises: a POST is not idempotent, so a transient
// failure is reported rather than retried into a duplicate notification.
func TestAFailedPostIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	if err := NewApprise(srv.URL, Options{}).Send(t.Context(), Notification{Title: "t"}); err == nil {
		t.Fatal("want the 502 reported")
	}
	if hits.Load() != 1 {
		t.Errorf("requests = %d, want exactly 1", hits.Load())
	}
}

// A cancelled context must abort the send rather than run to its own timeout.
func TestSendHonoursContextCancellation(t *testing.T) {
	srv, _ := recorder(t, http.StatusOK)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := NewWebhook(srv.URL, "", Options{}).Send(ctx, Notification{Title: "t"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func BenchmarkWebhookMarshal(b *testing.B) {
	srv, _ := recorder(&testing.T{}, http.StatusOK)
	defer srv.Close()
	s := NewWebhook(srv.URL, "secret", Options{})
	n := Notification{Title: "Backup complete", Body: "3 files", Level: LevelSuccess}
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		_ = s.Send(ctx, n)
	}
}
