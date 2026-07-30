package netx

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// stubPortLookup replaces portLookup for the duration of the test, so no test
// here ever shells out to lsof.
func stubPortLookup(t *testing.T, holder string, pid int) {
	t.Helper()
	prev := portLookup
	portLookup = func(int) (string, int) { return holder, pid }
	t.Cleanup(func() { portLookup = prev })
}

func TestAddrInUseHint_IgnoresUnrelatedErrors(t *testing.T) {
	// portLookup must never even be consulted for an error that isn't
	// EADDRINUSE — calling it here would fail the test.
	prev := portLookup
	portLookup = func(int) (string, int) {
		t.Fatal("portLookup should not be called for a non-EADDRINUSE error")
		return "", 0
	}
	t.Cleanup(func() { portLookup = prev })

	original := errors.New("some other failure")
	got := AddrInUseHint(original, 8080, "PORT")

	assert.Same(t, original, got, "a non-EADDRINUSE error must be returned untouched")
}

func TestAddrInUseHint_NamesPortAndEnvVarWithNoHolderFound(t *testing.T) {
	// Absent lookup (e.g. lsof missing, or nothing found) must still produce a
	// usable, generic hint — never a blank or missing message.
	stubPortLookup(t, "", 0)

	err := fmt.Errorf("listen tcp :8080: %w", syscall.EADDRINUSE)
	got := AddrInUseHint(err, 8080, "PORT")

	assert.Error(t, got)
	assert.Contains(t, got.Error(), "8080")
	assert.Contains(t, got.Error(), "PORT")
}

func TestAddrInUseHint_NamesHolderProcessWhenFound(t *testing.T) {
	stubPortLookup(t, "api (pid 5201)", 5201)

	err := fmt.Errorf("listen tcp :9090: %w", syscall.EADDRINUSE)
	got := AddrInUseHint(err, 9090, "METRICS_PORT")

	msg := got.Error()
	assert.Contains(t, msg, "9090")
	assert.Contains(t, msg, "METRICS_PORT")
	assert.Contains(t, msg, "api (pid 5201)")
	assert.Contains(t, msg, "kill 5201")
}
