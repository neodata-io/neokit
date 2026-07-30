package safe_test

import (
	"testing"
	"time"

	"github.com/neodata-io/neokit/safe"
)

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
