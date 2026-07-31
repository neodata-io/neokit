package pubsub

import (
	"sync"
	"testing"
	"time"
)

type event struct{ Kind string }

func TestPublishDeliversToEverySubscriberOfTheTopic(t *testing.T) {
	b := New[event](4)
	a, unsubA := b.Subscribe("room-1")
	defer unsubA()
	c, unsubC := b.Subscribe("room-1")
	defer unsubC()

	b.Publish("room-1", event{Kind: "ping"})

	for i, ch := range []<-chan event{a, c} {
		select {
		case got := <-ch:
			if got.Kind != "ping" {
				t.Errorf("subscriber %d got %+v", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

func TestTopicsAreIsolated(t *testing.T) {
	b := New[event](4)
	mine, unsub := b.Subscribe("room-1")
	defer unsub()

	b.Publish("room-2", event{Kind: "not-for-me"})

	select {
	case got := <-mine:
		t.Fatalf("received another topic's event: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
}

// Publishing to a topic nobody is listening to is the common case: an event
// fires whether or not a tab is open.
func TestPublishToAnEmptyTopicIsANoOp(t *testing.T) {
	b := New[event](1)
	b.Publish("nobody-here", event{Kind: "x"}) // must not block or panic
	if b.Topics() != 0 {
		t.Errorf("Topics = %d, want 0", b.Topics())
	}
}

// The trade this package makes: a subscriber that is not reading loses events
// rather than stalling the publisher, which is usually holding a lock.
func TestASlowSubscriberDropsRatherThanBlocksThePublisher(t *testing.T) {
	b := New[event](1)
	_, unsub := b.Subscribe("room-1")
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 { // far past the buffer of 1
			b.Publish("room-1", event{Kind: "flood"})
		}
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a subscriber that was not reading")
	}
}

// Not a theoretical nicety: the shape this replaced closed the channel directly,
// so an ordinary `defer unsub()` plus an explicit unsubscribe on client
// disconnect closed it twice and panicked inside an SSE handler.
func TestUnsubscribeIsIdempotent(t *testing.T) {
	b := New[event](1)
	_, unsub := b.Subscribe("room-1")
	unsub()
	unsub() // must not panic on an already-closed channel
	unsub()
}

// Closing on unsubscribe is the signal a `for range ch` consumer needs to
// terminate cleanly.
func TestUnsubscribeClosesTheChannel(t *testing.T) {
	b := New[event](1)
	ch, unsub := b.Subscribe("room-1")
	unsub()

	select {
	case _, open := <-ch:
		if open {
			t.Error("want the channel closed")
		}
	case <-time.After(time.Second):
		t.Fatal("the channel was never closed")
	}
}

// A departed topic must not leave an empty map behind, or a bus keyed on
// per-user tokens grows without bound.
func TestTheLastUnsubscribeReleasesTheTopic(t *testing.T) {
	b := New[event](1)
	_, a := b.Subscribe("room-1")
	_, c := b.Subscribe("room-1")

	a()
	if got := b.Subscribers("room-1"); got != 1 {
		t.Errorf("Subscribers = %d, want 1", got)
	}
	c()
	if got := b.Topics(); got != 0 {
		t.Errorf("Topics = %d after the last unsubscribe, want 0 — the topic map leaked", got)
	}
}

func TestNonPositiveBufferUsesTheDefault(t *testing.T) {
	for _, size := range []int{0, -1} {
		b := New[event](size)
		if b.buffer != DefaultBuffer {
			t.Errorf("New(%d).buffer = %d, want %d", size, b.buffer, DefaultBuffer)
		}
	}
}

// Publish, Subscribe and unsubscribe race constantly in an SSE server: every
// connect and disconnect touches the same map a broadcast is reading.
func TestConcurrentPublishSubscribeIsRaceFree(t *testing.T) {
	b := New[event](8)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish("room-1", event{Kind: "tick"})
				}
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				ch, unsub := b.Subscribe("room-1")
				select {
				case <-ch:
				default:
				}
				unsub()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// ── Benchmarks ──────────────────────────────────────────────────────────────

// Publish is called from request handlers and while holding locks, so its cost
// is the number that matters.
func BenchmarkPublishNoSubscribers(b *testing.B) {
	bus := New[event](16)
	b.ReportAllocs()
	for b.Loop() {
		bus.Publish("room-1", event{Kind: "x"})
	}
}

func BenchmarkPublishOneSubscriber(b *testing.B) {
	bus := New[event](1 << 16)
	ch, unsub := bus.Subscribe("room-1")
	defer unsub()
	go func() {
		for range ch {
		}
	}()
	b.ReportAllocs()
	for b.Loop() {
		bus.Publish("room-1", event{Kind: "x"})
	}
}

func BenchmarkSubscribeUnsubscribe(b *testing.B) {
	bus := New[event](4)
	b.ReportAllocs()
	for b.Loop() {
		_, unsub := bus.Subscribe("room-1")
		unsub()
	}
}
