// Package cache provides a tiny in-memory TTL cache with per-key single-flight
// and stale-while-revalidate refreshes.
//
// It exists to collapse repeated home-tile reads — the same tile polled by
// several devices at once — into a single upstream call per window, protecting
// rate-limited vendor clouds and making tiles feel instant.
package cache

import (
	"context"
	"sync"
	"time"

	"github.com/neodata-io/neokit/safe"
)

// defaultMaxEntries bounds the number of distinct keys a cache retains. Some
// callers key on user input (place-search queries, reverse-geocode coordinates),
// so without a cap a long-running process leaks one entry per unique input.
// Eviction is FIFO by insertion; the TTL still governs freshness of what's kept.
const defaultMaxEntries = 4096

// fetchTimeout is a backstop on any single fetch, not a latency budget: every
// upstream already bounds itself (the monitor gives each health check 5s, the
// plugin HTTP clients carry their own deadlines). It exists so a fetch that hangs
// on a socket nothing will ever answer cannot pin a background goroutine — or a
// request — forever.
const fetchTimeout = 20 * time.Second

// retryBackoff is how long a *failed* revalidation keeps serving the stale value
// before another is attempted. Without it a hard-down upstream would be re-dialled
// by every single request, since the value stays expired: the failure would turn
// the cache from a shield into an amplifier.
const retryBackoff = 5 * time.Second

// Cache stores values by key. Concurrent misses for the same key result in a
// single fetch; the other callers wait and share its result (single-flight).
type Cache struct {
	mu         sync.Mutex
	entries    map[string]*entry
	order      []string // insertion order of live keys, for FIFO eviction
	maxEntries int
}

type entry struct {
	// mu guards the fields below and is held only for the short critical sections
	// that read or publish them — never across a fetch. That is what lets a stale
	// read return immediately while a refresh runs behind it.
	mu      sync.Mutex
	val     any
	expires time.Time
	loaded  bool

	// fetch serialises the *synchronous* cold path, so concurrent callers for a key
	// that has no value yet queue up and share one fetch rather than stampeding the
	// upstream. Revalidation of an already-loaded key doesn't use it — that path is
	// gated by `refreshing` instead, and never makes anyone wait.
	fetch sync.Mutex

	// refreshing is true while a background revalidation is in flight, so a burst of
	// reads on a stale key spawns exactly one refresh rather than one per request.
	refreshing bool

	// gen increments on every Invalidate. A fetch captures it before starting and
	// discards its result if it changed meanwhile: the value was read *before* the
	// mutation that invalidated the key, so publishing it would reinstate exactly
	// the state the caller invalidated to get rid of.
	gen uint64
}

// New creates an empty cache with the default key cap.
func New() *Cache {
	return &Cache{entries: make(map[string]*entry), maxEntries: defaultMaxEntries}
}

func (c *Cache) entryFor(key string) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		return e
	}
	e := &entry{}
	c.entries[key] = e
	c.order = append(c.order, key)
	// Evict the oldest keys once over the cap. A goroutine still fetching an
	// evicted key keeps its *entry via the reference it already holds; the entry
	// simply won't be found by later lookups, which just causes a refetch.
	for len(c.order) > c.maxEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	return e
}

// Len reports how many distinct keys the cache is holding, live or stale.
//
// It exists for callers that need to assert on cache *occupancy* rather than on
// a value — notably any endpoint whose key derives from unauthenticated input,
// where "did this request consume an entry" is the property worth pinning
// (see the link-icon handler). Counting keys, not live entries: an expired entry
// still occupies a slot until it is evicted, which is what the cap governs.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Invalidate drops a key's value so the next GetOrFetch refetches it
// synchronously — used after a command mutates the underlying state, where the
// caller needs to *see* its own change rather than the value from before it.
//
// This is deliberately the one path that still makes a caller wait. Serving a
// stale value here would show the household the state it just changed away from,
// which reads as the command having failed.
func (c *Cache) Invalidate(key string) {
	e := c.entryFor(key)
	e.mu.Lock()
	e.loaded = false
	e.gen++ // discard any fetch already in flight; its value predates the mutation
	e.mu.Unlock()
}

// GetOrFetch returns the cached value for key, refreshing it through fn as needed.
//
// There are three cases, and only one of them makes the caller wait:
//
//   - Fresh — returned immediately.
//   - Stale — the previous value is returned immediately and a single background
//     revalidation is started. Nobody waits on a TTL boundary. This is what keeps
//     a slow upstream from periodically stalling every viewer at once: the sweep
//     behind /api/access/status costs seconds when the television is off, and
//     before this it was paid on the request path, by whoever happened to poll
//     first, while everyone else queued behind the same lock.
//   - Cold (never loaded, or just invalidated) — fn runs synchronously, once, with
//     concurrent callers queueing and sharing the result.
//
// fn receives a context owned by the cache, NOT the caller's. That is load-bearing
// twice over: a background revalidation outlives the request that triggered it, so
// a request-scoped context would be cancelled before it finished and the cache
// would never repopulate; and because single-flight shares one result among many
// callers, one impatient client disconnecting must not cancel the fetch everyone
// else is waiting on.
//
// Errors are NOT cached. On the cold path fn's value is still returned alongside
// the error, so a caller that builds a usable "degraded" value (e.g. a
// configured-but-unreachable view) can hand it back without that failure sticking
// for the full TTL. A failed *revalidation* keeps serving the last good value and
// retries after retryBackoff.
func GetOrFetch[T any](c *Cache, key string, ttl time.Duration, fn func(context.Context) (T, error)) (T, error) {
	e := c.entryFor(key)

	e.mu.Lock()
	if e.loaded {
		v := e.val.(T)
		if time.Now().Before(e.expires) {
			e.mu.Unlock()
			return v, nil // fresh
		}
		// Stale: hand back what we have and refresh behind it. `refreshing` is the
		// gate — a burst of reads on a stale key spawns one refresh, not one each.
		if !e.refreshing {
			e.refreshing = true
			go revalidate(e, key, ttl, fn)
		}
		e.mu.Unlock()
		return v, nil
	}
	e.mu.Unlock()

	// Cold. Queue behind any caller already fetching this key.
	e.fetch.Lock()
	defer e.fetch.Unlock()

	// Re-check under the fetch gate: whoever we queued behind has published by now.
	e.mu.Lock()
	if e.loaded {
		v := e.val.(T)
		e.mu.Unlock()
		return v, nil
	}
	gen := e.gen
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	v, err := fn(ctx)
	if err != nil {
		return v, err // hand the value back but don't cache the failure
	}

	e.mu.Lock()
	if e.gen == gen { // not invalidated while we fetched
		e.val = v
		e.expires = time.Now().Add(ttl)
		e.loaded = true
	}
	e.mu.Unlock()
	return v, nil
}

// revalidate refreshes an already-loaded key in the background. It never blocks a
// caller: readers are served the stale value while this runs.
func revalidate[T any](e *entry, key string, ttl time.Duration, fn func(context.Context) (T, error)) {
	e.mu.Lock()
	gen := e.gen
	e.mu.Unlock()

	var v T
	ok := false
	// The fetch is wrapped so a panicking upstream can't escape this detached
	// goroutine (which runs outside Fiber's recover middleware) and can't leave
	// `refreshing` stuck true, which would wedge the key on its stale value forever.
	// A panic leaves ok false, so it is handled exactly like a failed fetch.
	func() {
		defer safe.Recover("cache.revalidate " + key)
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		got, err := fn(ctx)
		if err == nil {
			v, ok = got, true
		}
	}()

	e.mu.Lock()
	e.refreshing = false
	switch {
	case e.gen != gen:
		// Invalidated while we fetched: this value predates the mutation, so drop it
		// and leave the key cold for the next caller to fetch synchronously.
	case ok:
		e.val = v
		e.expires = time.Now().Add(ttl)
		e.loaded = true
	default:
		// Keep serving the last good value; try again after the backoff rather than
		// on the very next read.
		e.expires = time.Now().Add(retryBackoff)
	}
	e.mu.Unlock()
}
