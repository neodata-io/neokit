// Package capset resolves optional capabilities out of a set of named
// components by type assertion, and memoises the answer.
//
// It is the machinery behind the "one required method, everything else
// optional" component shape: a component satisfies a small base interface, and
// any extra interface it happens to implement is a capability the caller
// discovers at runtime. The appeal is that adding a capability touches only the
// component that offers it and the caller that wants it — nothing in between.
//
// The trap is that discovery gets written by hand. Each capability grows a field
// on a registry struct, a line in its constructor, an `if c, ok := p.(Cap); ok`
// in a scan loop, and an accessor — four edits to shared code, per capability,
// forever, contradicting the promise that a component touches only its own
// package.
//
// [Set] says it once. Callers still write named accessors — those keep the
// capability list greppable and stop type assertions leaking into handlers — but
// each is a one-line delegation, and adding a capability touches no constructor:
//
//	func (r *Registry) Chargers() []Charger { return capset.All[Charger](r.set) }
//
// # Cost
//
// Assertions run once per capability, lazily, and are cached. A set must
// therefore be **immutable** once built: rebuild and swap rather than mutate
// (see [Set.Items]). Steady state is a map lookup under a read lock, plus a copy
// for [All] and [ByName] so no caller can change the shared cache.
package capset

import (
	"maps"
	"slices"
	"sync"
)

// Set is an immutable collection of components with cached capability lookups.
// Construct with [New]. Safe for concurrent use.
type Set[E any] struct {
	items []E
	name  func(E) string

	mu     sync.RWMutex
	lists  map[any]any // keyOf[C] → []C
	tables map[any]any // keyOf[C] → map[string]C
}

// New builds a set over items. name extracts a component's stable identifier and
// is used only by [ByName] and [Of]; it may be nil if neither is used.
//
// items must not be mutated afterwards — the caches assume the membership is
// fixed. To reflect a configuration change, build a new Set and swap it in
// (an atomic.Pointer[Set[E]] is the usual carrier).
func New[E any](items []E, name func(E) string) *Set[E] {
	return &Set[E]{
		items:  items,
		name:   name,
		lists:  make(map[any]any),
		tables: make(map[any]any),
	}
}

// Items returns the components the set was built from, in registration order.
// The slice is the set's own — treat it as read-only.
func (s *Set[E]) Items() []E { return s.items }

// Len reports how many components the set holds.
func (s *Set[E]) Len() int { return len(s.items) }

// keyOf names capability C for the memo without reflection: a nil *C boxed in
// an interface compares equal only to another nil *C of the same C.
func keyOf[C any]() any { return (*C)(nil) }

// All returns every component implementing capability C, in registration order.
// Registration order is stable and load-bearing for [First]. The slice is a
// copy, so a caller may change it.
//
// It is a free function rather than a method because Go does not allow a method
// to introduce its own type parameter — the capability has to come from
// somewhere, and this is the only place it can.
func All[C any, E any](s *Set[E]) []C { return slices.Clone(all[C](s)) }

// all is [All] without the copy, for callers in this package that only read.
func all[C any, E any](s *Set[E]) []C {
	key := keyOf[C]()

	s.mu.RLock()
	cached, ok := s.lists[key]
	s.mu.RUnlock()
	if ok {
		return cached.([]C)
	}

	var out []C
	for _, it := range s.items {
		if c, ok := any(it).(C); ok {
			out = append(out, c)
		}
	}

	s.mu.Lock()
	s.lists[key] = out
	s.mu.Unlock()
	return out
}

// ByName maps each implementor of C to its component's name. The map is a copy,
// so prefer [Of] for a single lookup.
//
// Panics if the set was built with a nil name function.
func ByName[C any, E any](s *Set[E]) map[string]C { return maps.Clone(byName[C](s)) }

// byName is [ByName] without the copy, for callers in this package that only read.
func byName[C any, E any](s *Set[E]) map[string]C {
	key := keyOf[C]()

	s.mu.RLock()
	cached, ok := s.tables[key]
	s.mu.RUnlock()
	if ok {
		return cached.(map[string]C)
	}
	if s.name == nil {
		panic("capset: ByName/Of require a name function (New was called with nil)")
	}

	out := make(map[string]C)
	for _, it := range s.items {
		if c, ok := any(it).(C); ok {
			out[s.name(it)] = c
		}
	}

	s.mu.Lock()
	s.tables[key] = out
	s.mu.Unlock()
	return out
}

// Of returns the named component's implementor of C, or the zero value — a nil
// interface — when it has none.
//
// That zero is the contract: "non-nil means available" is what every caller
// branches on, so a missing capability reads as an absent one rather than
// needing a second return value at every call site.
func Of[C any, E any](s *Set[E], name string) C {
	return byName[C](s)[name]
}

// First returns the first component implementing C, or the zero value when none
// does. For capabilities that are singletons by nature — one clock, one primary
// media source — first-registered wins, which makes registration order the
// tiebreak.
func First[C any, E any](s *Set[E]) C {
	var zero C
	if list := all[C](s); len(list) > 0 {
		return list[0]
	}
	return zero
}

// Both returns the implementors of C that also implement D. Some capabilities
// are only meaningful in combination — a search provider that is also a player,
// so a result can actually be played.
//
// Deliberately uncached: the pair space is quadratic in the number of
// capabilities, while All's cache already does the expensive half.
func Both[C any, D any, E any](s *Set[E]) []C {
	var out []C
	for _, c := range all[C](s) {
		if _, ok := any(c).(D); ok {
			out = append(out, c)
		}
	}
	return out
}

// Any reports whether any component implements C. Cheaper to read at a call site
// than len(All[C](s)) > 0, and it says what the caller means.
func Any[C any, E any](s *Set[E]) bool { return len(all[C](s)) > 0 }
