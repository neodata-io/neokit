package delivery_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neodata-io/neokit/webpush"
	"github.com/neodata-io/neokit/webpush/delivery"
)

// vapidKeys is a real generated keypair. The tests that came before this package
// used placeholder strings, which made webpush-go fail during signing — so every
// assertion past that point was vacuous and the fan-out was never exercised at
// all. Generating a real pair is what makes these tests reach the wire.
func vapidKeys(t *testing.T) webpush.Keys {
	t.Helper()
	k, err := webpush.GenerateKeys()
	if err != nil {
		t.Fatalf("GenerateKeys: %v", err)
	}
	return k
}

// browserSub fabricates what a browser's PushSubscription.getKey() returns: an
// uncompressed P-256 point and a 16-byte auth secret, both base64url.
func browserSub(t *testing.T, endpoint string) webpush.Subscription {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate subscription key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("generate auth secret: %v", err)
	}
	return webpush.Subscription{
		Endpoint: endpoint,
		P256DH:   base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(auth),
	}
}

type fakeStore struct {
	mu      sync.Mutex
	subs    []webpush.Subscription
	deleted []string
	listErr error
}

func (f *fakeStore) ListSubscriptions(context.Context, string) ([]webpush.Subscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subs, f.listErr
}

func (f *fakeStore) DeleteSubscription(_ context.Context, endpoint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, endpoint)
	return nil
}

func (f *fakeStore) deletedEndpoints() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// newSender builds a Sender whose transport reaches a loopback httptest TLS
// server. The real transport refuses loopback, so only newGuardedSender keeps it.
func newSender(t *testing.T, store delivery.Store, o delivery.Options) *delivery.Sender {
	t.Helper()
	s := newGuardedSender(t, store, o)
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // httptest's self-signed cert
	t.Cleanup(tr.CloseIdleConnections)
	s.HTTPClient().Transport = tr
	return s
}

// newGuardedSender builds a Sender exactly as production does.
func newGuardedSender(t *testing.T, store delivery.Store, o delivery.Options) *delivery.Sender {
	t.Helper()
	if o.Subject == "" {
		o.Subject = "ops@example.com"
	}
	s, err := delivery.New(store, vapidKeys(t), o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// captureLogs redirects the default logger for one test and returns its output.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// lockedBuffer is a bytes.Buffer safe for the fan-out's concurrent log writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The endpoint comes from the browser, so it is attacker-chosen: plain http is
// never a push service, and following it would POST into the local network.
func TestAPlainHTTPEndpointIsRefused(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	store := &fakeStore{subs: []webpush.Subscription{browserSub(t, srv.URL)}}
	delivered, err := newSender(t, store, delivery.Options{}).Send(t.Context(), "downloads", []byte(`{}`))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 0 || hits.Load() != 0 {
		t.Errorf("delivered = %d, requests = %d; a plain-http endpoint must never be contacted", delivered, hits.Load())
	}
}

func TestAMalformedEndpointIsRefused(t *testing.T) {
	logs := captureLogs(t)
	for _, endpoint := range []string{"", "not a url", "https://", "//push.example/x", "https://user:pw@push.example/x"} {
		store := &fakeStore{subs: []webpush.Subscription{browserSub(t, endpoint)}}
		delivered, err := newSender(t, store, delivery.Options{}).Send(t.Context(), "downloads", []byte(`{}`))
		if err != nil || delivered != 0 {
			t.Errorf("endpoint %q: delivered = %d, err = %v; want it refused", endpoint, delivered, err)
		}
	}
	if got := logs.String(); !strings.Contains(got, "refusing endpoint") || strings.Contains(got, "user:pw") {
		t.Errorf("want each refusal logged without the endpoint's credentials, got:\n%s", got)
	}
}

// The scheme check alone is not enough: an https URL can still name a box on
// the internal network, so the dial itself has to refuse it.
func TestTheDeliveryClientRefusesPrivateAddresses(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	client := newGuardedSender(t, &fakeStore{}, delivery.Options{}).HTTPClient()
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the delivery client reached a loopback address")
	}
	if hits.Load() != 0 {
		t.Errorf("loopback server saw %d requests, want 0", hits.Load())
	}
}

// A redirect would carry the VAPID JWT and the encrypted payload to a host the
// push service chose, past the endpoint check.
func TestARedirectIsNotFollowed(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	store := &fakeStore{subs: []webpush.Subscription{browserSub(t, srv.URL)}}
	delivered, err := newSender(t, store, delivery.Options{}).Send(t.Context(), "downloads", []byte(`{}`))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 0 || hits.Load() != 0 {
		t.Errorf("delivered = %d, redirect target requests = %d; want 0 and 0", delivered, hits.Load())
	}
}

// The per-device URL is a capability to push to that device. A transport error
// renders the whole URL, so it must be cut down to the host before it is logged.
func TestATransportFailureLogsOnlyTheEndpointHost(t *testing.T) {
	logs := captureLogs(t)
	dead := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := dead.URL + "/fcm/send/DEVICE-CAPABILITY-TOKEN"
	dead.Close()

	store := &fakeStore{subs: []webpush.Subscription{browserSub(t, endpoint)}}
	if _, err := newSender(t, store, delivery.Options{}).Send(t.Context(), "downloads", []byte(`{}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := logs.String()
	if !strings.Contains(got, "delivery failed") {
		t.Fatalf("want the failure logged, got:\n%s", got)
	}
	if strings.Contains(got, "DEVICE-CAPABILITY-TOKEN") {
		t.Errorf("the log leaked the per-device endpoint:\n%s", got)
	}
}

func TestSendDeliversToEverySubscriber(t *testing.T) {
	var mu sync.Mutex
	var bodies int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		bodies++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	store := &fakeStore{subs: []webpush.Subscription{
		browserSub(t, srv.URL), browserSub(t, srv.URL), browserSub(t, srv.URL),
	}}
	sender := newSender(t, store, delivery.Options{})

	delivered, err := sender.Send(t.Context(), "downloads", []byte(`{"title":"hi"}`))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 3 {
		t.Errorf("delivered = %d, want 3", delivered)
	}
	mu.Lock()
	defer mu.Unlock()
	if bodies != 3 {
		t.Errorf("push service saw %d requests, want 3", bodies)
	}
}

// 410 Gone is the push service saying the browser threw the subscription away.
// Keeping it would mean signing and sending to a dead endpoint on every push,
// forever.
func TestSendPrunesASubscriptionReportedGone(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	store := &fakeStore{subs: []webpush.Subscription{browserSub(t, srv.URL)}}
	sender := newSender(t, store, delivery.Options{})

	delivered, err := sender.Send(t.Context(), "downloads", []byte(`{}`))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
	if got := store.deletedEndpoints(); len(got) != 1 || got[0] != srv.URL {
		t.Errorf("deleted = %v, want [%s]", got, srv.URL)
	}
}

func TestSendPrunesASubscriptionReportedNotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	store := &fakeStore{subs: []webpush.Subscription{browserSub(t, srv.URL)}}
	sender := newSender(t, store, delivery.Options{})

	if _, err := sender.Send(t.Context(), "downloads", []byte(`{}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := store.deletedEndpoints(); len(got) != 1 {
		t.Errorf("a 404 must drop the subscription too, deleted = %v", got)
	}
}

// A push service that rejects the message (a bad JWT, a quota) is not saying the
// subscription is dead — dropping it there would silently unsubscribe a working
// device.
func TestARejectedDeliveryIsNotPruned(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	store := &fakeStore{subs: []webpush.Subscription{browserSub(t, srv.URL)}}
	sender := newSender(t, store, delivery.Options{})

	delivered, err := sender.Send(t.Context(), "downloads", []byte(`{}`))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
	if got := store.deletedEndpoints(); len(got) != 0 {
		t.Errorf("a rejected message must not unsubscribe the device, deleted = %v", got)
	}
}

// One dead device must not cost the household every other notification.
func TestOneFailingEndpointDoesNotStopTheRest(t *testing.T) {
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer good.Close()
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	store := &fakeStore{subs: []webpush.Subscription{
		browserSub(t, bad.URL), browserSub(t, good.URL), browserSub(t, good.URL),
	}}
	sender := newSender(t, store, delivery.Options{})

	delivered, err := sender.Send(t.Context(), "downloads", []byte(`{}`))
	if err != nil {
		t.Fatalf("a single failed delivery must not fail the fan-out: %v", err)
	}
	if delivered != 2 {
		t.Errorf("delivered = %d, want 2", delivered)
	}
}

func TestSendReportsAStoreFailure(t *testing.T) {
	sentinel := errors.New("database is locked")
	sender := newSender(t, &fakeStore{listErr: sentinel}, delivery.Options{})

	if _, err := sender.Send(t.Context(), "downloads", []byte(`{}`)); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to wrap %v", err, sentinel)
	}
}

func TestSendWithNoSubscribersTouchesNothing(t *testing.T) {
	sender := newSender(t, &fakeStore{}, delivery.Options{})

	delivered, err := sender.Send(t.Context(), "downloads", []byte(`{}`))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
}

// A stuck endpoint holds its delivery slot for the full timeout. Without a cap
// on how many run at once, a handful of dead devices starve the live ones.
func TestConcurrencyIsBounded(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		time.Sleep(40 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	var subs []webpush.Subscription
	for range 6 {
		subs = append(subs, browserSub(t, srv.URL))
	}
	sender := newSender(t, &fakeStore{subs: subs}, delivery.Options{Concurrency: 2})

	delivered, err := sender.Send(t.Context(), "downloads", []byte(`{}`))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if delivered != 6 {
		t.Errorf("delivered = %d, want 6", delivered)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Errorf("peak in-flight deliveries = %d, want at most 2", peak)
	}
}

// webpush-go's zero-value client has NO timeout, so a push endpoint that accepts
// the connection and then goes quiet would wedge the calling goroutine — which
// for a scheduler means that sampler is dead for the life of the process.
func TestASilentEndpointIsBoundedByTheTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	defer close(release)

	store := &fakeStore{subs: []webpush.Subscription{browserSub(t, srv.URL)}}
	sender := newSender(t, store, delivery.Options{Timeout: 50 * time.Millisecond})

	done := make(chan int, 1)
	go func() {
		n, _ := sender.Send(t.Context(), "downloads", []byte(`{}`))
		done <- n
	}()

	select {
	case n := <-done:
		if n != 0 {
			t.Errorf("delivered = %d, want 0 — the endpoint never answered", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not return: the per-delivery timeout is not being applied")
	}
}

// The trap this package exists to close: webpush-go prepends "mailto:" to any
// subject that is not an https URL, so a stored "mailto:ops@example.com" becomes
// "mailto:mailto:ops@example.com" — a malformed JWT that Apple rejects with a
// bare BadJwtToken naming no claim.
func TestAPrefixedSubjectProducesExactlyOneMailtoInTheJWT(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("Authorization"):
		default:
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	store := &fakeStore{subs: []webpush.Subscription{browserSub(t, srv.URL)}}
	sender := newSender(t, store, delivery.Options{Subject: "mailto:ops@example.com"})

	if _, err := sender.Send(t.Context(), "downloads", []byte(`{}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	sub := vapidSubject(t, <-got)
	if sub != "mailto:ops@example.com" {
		t.Errorf("JWT sub = %q, want exactly one mailto: prefix", sub)
	}
}

// vapidSubject digs the `sub` claim out of a `vapid t=<jwt>, k=<key>` header.
func vapidSubject(t *testing.T, authorization string) string {
	t.Helper()
	_, rest, ok := strings.Cut(authorization, "t=")
	if !ok {
		t.Fatalf("no VAPID token in %q", authorization)
	}
	token, _, _ := strings.Cut(rest, ",")
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT payload: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("unmarshal JWT payload: %v", err)
	}
	return claims.Sub
}

// A keypair is read back from storage on every boot. A half-written one signs
// every push with a key no browser accepts — 401 per subscription, per send,
// forever, with nothing to say why.
func TestNewRejectsAMalformedKeypair(t *testing.T) {
	_, err := delivery.New(&fakeStore{}, webpush.Keys{Public: "not-a-key", Private: "nope"},
		delivery.Options{Subject: "ops@example.com"})
	if !errors.Is(err, webpush.ErrInvalidKey) {
		t.Errorf("err = %v, want it to wrap ErrInvalidKey", err)
	}
}

func TestNewRejectsAnUnusableSubject(t *testing.T) {
	if _, err := delivery.New(&fakeStore{}, vapidKeys(t), delivery.Options{Subject: "not a contact"}); err == nil {
		t.Error("a subject that is neither an address nor an https URL must be refused")
	}
}

func TestPublicKeyIsServedForSubscribing(t *testing.T) {
	keys := vapidKeys(t)
	s, err := delivery.New(&fakeStore{}, keys, delivery.Options{Subject: "ops@example.com"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.PublicKey() != keys.Public {
		t.Errorf("PublicKey() = %q, want %q", s.PublicKey(), keys.Public)
	}
}
