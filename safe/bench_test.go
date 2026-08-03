package safe

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
)

func quiet(b *testing.B) {
	b.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(prev) })
}

// BenchmarkRecover_NoPanic is the cost every guarded goroutine pays on the
// overwhelmingly common path where nothing panics at all. A deferred recover
// that never fires should be close to free.
func BenchmarkRecover_NoPanic(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		func() {
			defer Recover("job")
		}()
	}
}

// BenchmarkRecover_NoPanic_DynamicName reflects how callers actually name a
// guarded run when the name carries a key ("cache.revalidate " + key). The
// concatenation is evaluated at the defer statement, so it is paid on every
// call whether or not a panic ever occurs — this measures that.
func BenchmarkRecover_NoPanic_DynamicName(b *testing.B) {
	key := "some-cache-key"
	b.ReportAllocs()
	for b.Loop() {
		func() {
			defer Recover("cache.revalidate " + key)
		}()
	}
}

// BenchmarkGo_SpawnAndReturn measures spawning a supervised goroutine that
// returns immediately, including the shared WaitGroup bookkeeping.
func BenchmarkGo_SpawnAndReturn(b *testing.B) {
	quiet(b)
	b.ReportAllocs()
	for b.Loop() {
		var wg sync.WaitGroup
		wg.Add(1)
		Go(context.Background(), "job", func() { wg.Done() })
		wg.Wait()
	}
}

// BenchmarkGo_SpawnParallel measures contention on the package-level WaitGroup
// when many goroutines are supervised concurrently.
func BenchmarkGo_SpawnParallel(b *testing.B) {
	quiet(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			var wg sync.WaitGroup
			wg.Add(1)
			Go(context.Background(), "job", func() { wg.Done() })
			wg.Wait()
		}
	})
}
