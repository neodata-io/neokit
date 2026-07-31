package cache

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// fetchInt is a trivial, instant fetch so these benchmarks measure the cache's
// own overhead rather than an upstream's latency.
func fetchInt(context.Context) (int, error) { return 42, nil }

// BenchmarkGetOrFetch_Hit_Serial is the floor: a single goroutine reading one
// hot key. This is what the cache costs when there is no contention at all.
func BenchmarkGetOrFetch_Hit_Serial(b *testing.B) {
	c := New()
	if _, err := GetOrFetch(c, "k", time.Hour, fetchInt); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := GetOrFetch(c, "k", time.Hour, fetchInt); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetOrFetch_Hit_Parallel_SameKey is the dashboard shape the package
// was written for: many concurrent readers of one hot key. Every one of them
// takes the cache-wide mutex in entryFor before it can reach the entry's own
// lock, so this measures whether that global lock is the ceiling.
func BenchmarkGetOrFetch_Hit_Parallel_SameKey(b *testing.B) {
	c := New()
	if _, err := GetOrFetch(c, "k", time.Hour, fetchInt); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := GetOrFetch(c, "k", time.Hour, fetchInt); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGetOrFetch_Hit_Parallel_DistinctKeys separates the two locks: with
// every goroutine on its own key there is no per-entry contention left, so any
// slowdown here is attributable to the single cache-wide mutex alone.
func BenchmarkGetOrFetch_Hit_Parallel_DistinctKeys(b *testing.B) {
	const keys = 64
	c := New()
	for i := range keys {
		if _, err := GetOrFetch(c, strconv.Itoa(i), time.Hour, fetchInt); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	var n int64
	b.RunParallel(func(pb *testing.PB) {
		n++
		key := strconv.Itoa(int(n) % keys)
		for pb.Next() {
			if _, err := GetOrFetch(c, key, time.Hour, fetchInt); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGetOrFetch_Miss_Cold measures the synchronous cold path, including
// the context.WithTimeout the cache creates to own the fetch's lifetime.
func BenchmarkGetOrFetch_Miss_Cold(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		c := New()
		if _, err := GetOrFetch(c, "k", time.Hour, fetchInt); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInvalidate measures the post-command path.
func BenchmarkInvalidate(b *testing.B) {
	c := New()
	if _, err := GetOrFetch(c, "k", time.Hour, fetchInt); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		c.Invalidate("k")
	}
}

// BenchmarkGetOrFetch_Hit_LargeValue shows what the any-boxing of the stored
// value costs when T is bigger than a word — the struct case, which is what
// most real cached views are.
type view struct {
	Name   string
	Count  int
	Items  []string
	Nested struct{ A, B, C int }
}

func fetchView(context.Context) (view, error) {
	return view{Name: "x", Count: 1, Items: []string{"a", "b"}}, nil
}

func BenchmarkGetOrFetch_Hit_LargeValue(b *testing.B) {
	c := New()
	if _, err := GetOrFetch(c, "k", time.Hour, fetchView); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := GetOrFetch(c, "k", time.Hour, fetchView); err != nil {
			b.Fatal(err)
		}
	}
}
