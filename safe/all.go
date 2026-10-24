package safe

import (
	"context"
	"sync"
	"time"
)

// All calls fn for every item concurrently — each on its own goroutine, under its
// own timeout and its own panic recovery — and returns the results in item order
// alongside an ok flag per item.
//
// ok[i] is false when fn for item i returned an error, ran out of time, or
// panicked; results[i] is then the zero value. That pair lets a caller tell
// "answered with nothing" from "never answered", which is the distinction a
// dashboard reading several vendor clouds at once needs: one slow or broken
// source must neither blank nor delay the others, and a view can still say
// which sources it heard from.
//
// Errors are deliberately not returned one by one. A source that fails is
// simply absent, which is how a best-effort fan-out wants it; when the first
// error should abort the rest, use errgroup instead.
//
// A timeout of zero or less applies no per-item bound beyond ctx itself. name
// labels a panic in the log.
func All[T, R any](ctx context.Context, items []T, timeout time.Duration, name string, fn func(context.Context, T) (R, error)) (results []R, ok []bool) {
	results = make([]R, len(items))
	ok = make([]bool, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Go(func() {
			// Detached from any request goroutine, so no recover middleware covers
			// it: an unguarded panic in one source would take the process down.
			runGuarded(nil, name, func() {
				ictx := ctx
				if timeout > 0 {
					var cancel context.CancelFunc
					ictx, cancel = context.WithTimeout(ctx, timeout)
					defer cancel()
				}
				r, err := fn(ictx, item)
				if err != nil {
					return // drop this source; the others still answer
				}
				results[i] = r
				ok[i] = true
			})
		})
	}
	wg.Wait()
	return results, ok
}
