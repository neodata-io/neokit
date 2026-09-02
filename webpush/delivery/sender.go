// Package delivery sends Web Push messages to the browsers subscribed to a
// topic, and prunes the ones that have gone away.
//
// It is the half of Web Push that [github.com/neodata-io/neokit/webpush] leaves
// out, kept in a subpackage for one reason: encryption and delivery need a real
// implementation, and pulling one in should not be the price of generating a
// keypair. A caller that only mints and stores VAPID keys imports the parent and
// links nothing extra.
//
// What is actually here is the handful of decisions that separate a fan-out that
// works from one that quietly rots:
//
//   - a bounded number of deliveries in flight, because a dead device holds its
//     slot for the whole timeout and would otherwise starve the live ones;
//   - an explicit per-delivery timeout, because the underlying client's default
//     is none at all, and a caller is typically a scheduler goroutine that would
//     wedge for the life of the process;
//   - 404/410 means the browser threw the subscription away, so it is dropped —
//     while any other rejection is left alone, since unsubscribing a working
//     device over a transient 403 is the worse failure;
//   - one failed delivery never fails the others, and never fails the call.
//
// The payload is opaque bytes. What a service worker expects inside it — title,
// body, tag, an icon, a URL to open — is the application's contract with its own
// front end, not something this package should have opinions about.
package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	webpushgo "github.com/SherClockHolmes/webpush-go"

	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/safe"
	"github.com/neodata-io/neokit/webpush"
)

// Store is the subscription persistence a Sender needs: the subscribers for a
// topic, and a way to retire one the push service has declared dead.
//
// Only the two methods the fan-out uses are here. Saving a subscription and
// reading one back are the application's business — a handler receives them from
// the browser — and a Sender never does either.
type Store interface {
	// ListSubscriptions returns the subscribers that should receive a message
	// published to topic. What a topic means, and whether a subscriber opted
	// into it, is entirely the store's business; a Sender only passes it along.
	ListSubscriptions(ctx context.Context, topic string) ([]webpush.Subscription, error)
	// DeleteSubscription retires the subscription with this endpoint. It is
	// called when a push service reports the browser has thrown it away.
	DeleteSubscription(ctx context.Context, endpoint string) error
}

// Defaults applied to a zero-valued [Options].
const (
	// DefaultTTL is how long a push service should hold an undelivered message.
	// A day: long enough that a phone which was off overnight still gets it.
	DefaultTTL = 86400

	// DefaultConcurrency bounds deliveries in flight. A household has a handful
	// of devices, but a stuck endpoint holds its slot for the full timeout, so
	// the cap is what stops one dead device starving the rest — while still not
	// opening an unbounded number of sockets.
	DefaultConcurrency = 8

	// DefaultTimeout bounds one delivery. The underlying client's own default is
	// no timeout at all.
	DefaultTimeout = 10 * time.Second
)

// Options configures a Sender. Only Subject is required.
type Options struct {
	// Subject is the VAPID `sub` contact: the address a push service would use
	// to reach the operator about a misbehaving sender. Required, and passed
	// through [webpush.NormalizeSubject], so it may be given with or without a
	// "mailto:" prefix.
	Subject string
	// TTL is how many seconds a push service should hold an undelivered
	// message. Zero means [DefaultTTL].
	TTL int
	// Concurrency bounds deliveries in flight. Zero or negative means
	// [DefaultConcurrency].
	Concurrency int
	// Timeout bounds one delivery. Zero or negative means [DefaultTimeout].
	Timeout time.Duration
}

// Sender delivers Web Push messages to a topic's subscribers.
type Sender struct {
	store   Store
	keys    webpush.Keys
	subject string
	ttl     int
	limit   int
	client  *http.Client
}

// New builds a Sender.
//
// It validates the keypair and normalises the subject up front, so the two
// failures that are otherwise invisible in production are caught at wiring time:
// a half-written keypair read back from storage signs every push with a key no
// browser accepts (401 per subscription, per send, forever), and a subject that
// already carries its "mailto:" prefix produces "mailto:mailto:…", which Apple
// rejects with a bare BadJwtToken naming no claim.
func New(store Store, keys webpush.Keys, o Options) (*Sender, error) {
	if err := keys.Validate(); err != nil {
		return nil, err
	}
	subject, err := webpush.NormalizeSubject(o.Subject)
	if err != nil {
		return nil, err
	}
	ttl := o.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	limit := o.Concurrency
	if limit <= 0 {
		limit = DefaultConcurrency
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Sender{
		store:   store,
		keys:    keys,
		subject: subject,
		ttl:     ttl,
		limit:   limit,
		// One client, shared: http.Client is safe for concurrent use, and the
		// timeout is the same for every delivery. webpush-go left to itself uses
		// a client with no timeout at all.
		client: &http.Client{Timeout: timeout},
	}, nil
}

// PublicKey returns the VAPID application server key a browser passes to
// pushManager.subscribe(). It is public information; serve it freely.
func (s *Sender) PublicKey() string { return s.keys.Public }

// Send delivers payload to every subscriber of topic, and reports how many the
// push services accepted.
//
// The error is about *listing*, not delivering: an individual delivery failure
// is logged and counted out, never returned, because one unreachable device is
// the normal case and there is nothing a caller would do differently about it.
// Compare the returned count against what you expected if you need to know.
func (s *Sender) Send(ctx context.Context, topic string, payload []byte) (int, error) {
	subs, err := s.store.ListSubscriptions(ctx, topic)
	if err != nil {
		return 0, fmt.Errorf("webpush: list subscriptions: %w", err)
	}
	return s.SendTo(ctx, subs, payload)
}

// SendTo delivers payload to an explicit set of subscribers, for the cases a
// topic does not describe — a test notification to every registered device,
// say. It is what [Send] runs once it has its list.
func (s *Sender) SendTo(ctx context.Context, subs []webpush.Subscription, payload []byte) (int, error) {
	if len(subs) == 0 {
		return 0, nil
	}

	options := &webpushgo.Options{
		Subscriber:      s.subject,
		VAPIDPublicKey:  s.keys.Public,
		VAPIDPrivateKey: s.keys.Private,
		TTL:             s.ttl,
		HTTPClient:      s.client,
	}

	// payload and options are read-only here and the store serialises its own
	// writes, so the deliveries are independent; only the tally is shared.
	var delivered atomic.Int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.limit)
	for _, sub := range subs {
		wg.Add(1)
		sem <- struct{}{} // blocks once s.limit deliveries are in flight
		go func(sub webpush.Subscription) {
			defer wg.Done()
			defer func() { <-sem }()
			// Callers are typically schedulers, whose goroutines are detached
			// from any HTTP middleware: an unguarded panic in the delivery
			// library would take the process down, not just one push.
			defer safe.Recover("webpush: deliver")
			if s.deliver(ctx, sub, payload, options) {
				delivered.Add(1)
			}
		}(sub)
	}
	wg.Wait()

	n := int(delivered.Load())
	slog.Debug("webpush: fan-out complete", "subscriptions", len(subs), "delivered", n)
	return n, nil
}

// deliver sends one message and reports whether the push service accepted it.
// Safe to call concurrently.
func (s *Sender) deliver(ctx context.Context, sub webpush.Subscription, payload []byte, o *webpushgo.Options) bool {
	resp, err := webpushgo.SendNotificationWithContext(ctx, payload, &webpushgo.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpushgo.Keys{Auth: sub.Auth, P256dh: sub.P256DH},
	}, o)
	if err != nil {
		// Could not reach the push service at all.
		slog.Warn("webpush: delivery failed", "endpoint", endpointHost(sub.Endpoint), logx.Err(err))
		return false
	}
	// The body is only used to log a rejection reason, so bound the read: an
	// unexpected oversized response should not amplify memory or flood the log,
	// and a bounded prefix still drains enough for connection reuse.
	body, _ := readLimited(resp)
	resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound:
		// The browser threw this subscription away. Retire it, or every future
		// push signs and sends to a dead endpoint.
		slog.Info("webpush: dropping expired subscription",
			"endpoint", endpointHost(sub.Endpoint), "status", resp.StatusCode)
		if err := s.store.DeleteSubscription(ctx, sub.Endpoint); err != nil {
			slog.Warn("webpush: remove expired subscription", logx.Err(err))
		}
		return false
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true
	default:
		// Accepted the request, rejected the message — a bad VAPID subject, a
		// quota. NOT a dead subscription: dropping it here would silently
		// unsubscribe a device that works.
		slog.Warn("webpush: rejected by push service",
			"endpoint", endpointHost(sub.Endpoint),
			"status", resp.StatusCode,
			"body", strings.TrimSpace(string(body)))
		return false
	}
}

// maxLoggedBody bounds how much of a rejection body reaches the log.
const maxLoggedBody = 1 << 10

func readLimited(resp *http.Response) ([]byte, error) {
	buf := make([]byte, maxLoggedBody)
	n, err := resp.Body.Read(buf)
	return buf[:n], err
}

// endpointHost trims a push endpoint to its host for logging — enough to tell
// Apple, FCM and Mozilla apart without dumping the long per-device URL, which is
// a capability to push to that device.
func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}
