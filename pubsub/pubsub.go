// Package pubsub is an in-process, topic-keyed fan-out for pushing events at
// connected clients — Server-Sent Events and WebSocket streams, where each
// browser subscribes to one topic (a user id, a job token, a room) and wants
// only that topic's events.
//
// It is deliberately not a message queue. There is no persistence, no delivery
// guarantee, and no backpressure: a subscriber that is not reading fast enough
// **loses events**, silently, rather than blocking the publisher. That is the
// right trade for a live UI feed — the publisher is usually holding a lock or
// mid-request, and one wedged browser must not be able to stall it — and the
// wrong trade for anything where a dropped event matters. Use a real queue for
// those.
package pubsub

import "sync"

// DefaultBuffer is the per-subscriber queue depth used by [New] when given a
// non-positive size. It absorbs an ordinary burst without dropping while still
// bounding the memory one stalled subscriber can pin.
const DefaultBuffer = 16

// Bus fans values out to the subscribers of a topic. The zero value is not
// usable; construct with [New]. Safe for concurrent use.
type Bus[T any] struct {
	buffer int

	mu   sync.RWMutex
	subs map[string]map[chan T]struct{}
}

// New builds a bus whose subscriber channels hold buffer values. A non-positive
// buffer uses [DefaultBuffer].
func New[T any](buffer int) *Bus[T] {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	return &Bus[T]{buffer: buffer, subs: make(map[string]map[chan T]struct{})}
}

// Publish delivers v to every current subscriber of topic, skipping any whose
// buffer is full. It never blocks, so it is safe to call while holding a lock or
// from inside a request handler.
//
// Publishing to a topic with no subscribers is free and is the common case: an
// event fires whether or not anyone has a tab open.
func (b *Bus[T]) Publish(topic string, v T) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs[topic] {
		select {
		case ch <- v:
		default: // slow subscriber — drop, see the package doc
		}
	}
}

// Subscribe returns a channel of the topic's events and a function that
// unsubscribes and closes it.
//
// The channel is closed by unsubscribe, so `for v := range ch` is the natural
// consumer and terminates cleanly. Always call the returned function — the
// usual `defer unsubscribe()` — or the subscription leaks for the life of the
// process and the bus keeps filling a channel nobody reads.
//
// Unsubscribing twice is safe, which matters because the ordinary combination of
// a `defer unsubscribe()` and an explicit early unsubscribe on client disconnect
// would otherwise close the channel twice and panic inside the SSE handler.
func (b *Bus[T]) Subscribe(topic string) (<-chan T, func()) {
	ch := make(chan T, b.buffer)

	b.mu.Lock()
	if b.subs[topic] == nil {
		b.subs[topic] = make(map[chan T]struct{})
	}
	b.subs[topic][ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs[topic], ch)
			if len(b.subs[topic]) == 0 {
				delete(b.subs, topic) // don't leak an empty map per departed topic
			}
			b.mu.Unlock()

			// Closed after the map delete, and after releasing the lock: any
			// Publish still holding the read lock finished before Lock was granted,
			// and any Publish after it cannot see this channel. So no send can race
			// this close.
			close(ch)
		})
	}
}

// Subscribers reports how many live subscriptions a topic has. Intended for
// metrics and tests — a handler should not branch on it, since it can change the
// instant after it is read.
func (b *Bus[T]) Subscribers(topic string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[topic])
}

// Topics reports how many topics currently have at least one subscriber.
func (b *Bus[T]) Topics() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
