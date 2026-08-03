package safe_test

import (
	"errors"
	"testing"
	"time"

	"github.com/neodata-io/neokit/safe"
)

// A drain that gave up has to say so: app.Run turns it into a failed shutdown
// step, and a process that abandoned background work must not exit zero.
func TestWaitGoReportsATimeout(t *testing.T) {
	release := make(chan struct{})
	// Drain the straggler before leaving, or the next test's WaitGo waits on it.
	t.Cleanup(func() { close(release); safe.WaitGo(2 * time.Second) })

	safe.Go("straggler", func() { <-release })

	if err := safe.WaitGo(50 * time.Millisecond); !errors.Is(err, safe.ErrDrainTimeout) {
		t.Errorf("WaitGo err = %v, want %v", err, safe.ErrDrainTimeout)
	}
}

func TestGoRecoversPanicAndKeepsTheWaitGroupBalanced(t *testing.T) {
	// If Go failed to recover, the panic would escape its goroutine and take
	// the whole test binary down — so reaching the next line is the assertion.
	safe.Go("panicking-worker", func() { panic("boom") })
	safe.WaitGo(2 * time.Second)

	// The non-obvious failure mode: a recovered panic that skipped its
	// deferred Done() would leave the shared WaitGroup unbalanced, and every
	// later WaitGo would block until its timeout.
	ran := make(chan struct{})
	safe.Go("second-worker", func() { close(ran) })
	safe.WaitGo(2 * time.Second)

	select {
	case <-ran:
	default:
		t.Error("second Go did not complete: the shared WaitGroup was left unbalanced")
	}
}
