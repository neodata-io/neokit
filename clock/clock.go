// Package clock supplies the two implementations of a Now() time.Time
// dependency: the real one, and a fake a test drives by hand.
//
// There is deliberately no Clock interface here. The consumer declares it —
// one method, on its own side, naming only what it uses. What repeats across
// projects is not the interface but the fake, which every codebase otherwise
// rewrites once per test package.
package clock

import (
	"sync"
	"time"
)

// RealClock reads the wall clock. Use it in production wiring.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// Fake is a clock that only moves when a test moves it, so a test asserts on an
// exact instant instead of a tolerance around time.Now.
//
// Safe for concurrent use: code under test often reads the clock from a
// goroutine while the test advances it.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a Fake reading t.
func NewFake(t time.Time) *Fake { return &Fake{now: t} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d. A negative d moves it back, which is
// how you exercise a clock that has stepped backwards (NTP correction).
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// Set moves the clock to t.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}
