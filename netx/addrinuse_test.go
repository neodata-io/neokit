package netx

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLookup returns a Lookup that reports a fixed holder, so no test here ever
// shells out to lsof. It is passed in per call rather than swapped into package
// state, which is what lets these run in parallel.
func stubLookup(holder string, pid int) Lookup {
	return func(int) (string, int) { return holder, pid }
}

func TestAddrInUseHint_IgnoresUnrelatedErrors(t *testing.T) {
	t.Parallel()

	// The lookup must never even be consulted for an error that isn't
	// EADDRINUSE — calling it here would fail the test.
	never := Lookup(func(int) (string, int) {
		t.Error("lookup must not be called for a non-EADDRINUSE error")
		return "", 0
	})

	original := errors.New("some other failure")
	got := AddrInUseHint(original, 8080, "PORT", never)

	assert.Same(t, original, got, "a non-EADDRINUSE error must be returned untouched")
}

func TestAddrInUseHint_NamesPortAndEnvVarWithNoHolderFound(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("listen tcp :8080: %w", syscall.EADDRINUSE)
	got := AddrInUseHint(err, 8080, "PORT", NoLookup)

	require.Error(t, got)
	assert.Contains(t, got.Error(), "8080")
	assert.Contains(t, got.Error(), "PORT")
}

func TestAddrInUseHint_NamesHolderProcessWhenFound(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("listen tcp :9090: %w", syscall.EADDRINUSE)
	got := AddrInUseHint(err, 9090, "METRICS_PORT", stubLookup("api (pid 5201)", 5201))

	msg := got.Error()
	assert.Contains(t, msg, "9090")
	assert.Contains(t, msg, "METRICS_PORT")
	assert.Contains(t, msg, "api (pid 5201)")
	assert.Contains(t, msg, "kill 5201")
}

// Callers are told to route every listen error through this, so the cause has
// to survive: errors.Is must still reach EADDRINUSE downstream.
func TestAddrInUseHint_PreservesTheWrappedSyscallError(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("listen tcp :8080: %w", syscall.EADDRINUSE)

	withHolder := AddrInUseHint(err, 8080, "PORT", stubLookup("api (pid 1)", 1))
	assert.ErrorIs(t, withHolder, syscall.EADDRINUSE,
		"the hint must still unwrap to EADDRINUSE when a holder was identified")

	withoutHolder := AddrInUseHint(err, 8080, "PORT", NoLookup)
	assert.ErrorIs(t, withoutHolder, syscall.EADDRINUSE,
		"the hint must still unwrap to EADDRINUSE when no holder was found")
}

// A nil Lookup in the variadic slot must fall back to the default rather than
// panicking — the zero value of a caller's config field lands here.
func TestAddrInUseHint_NilLookupFallsBackToTheDefault(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("listen tcp :1: %w", syscall.EADDRINUSE)
	got := AddrInUseHint(err, 1, "PORT", nil)

	require.Error(t, got)
	assert.ErrorIs(t, got, syscall.EADDRINUSE)
}

// The default is [NoLookup], not [LsofLookup]: omitting the argument must not
// execute a subprocess. Asserting on the generic message is the observable
// proxy — a lookup that ran and found this process would name it and offer a
// kill command.
func TestAddrInUseHint_DefaultsToNoLookup(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("listen tcp :8080: %w", syscall.EADDRINUSE)
	got := AddrInUseHint(err, 8080, "PORT")

	require.Error(t, got)
	assert.ErrorIs(t, got, syscall.EADDRINUSE)
	assert.Contains(t, got.Error(), "stop whatever is listening on it")
	assert.NotContains(t, got.Error(), "kill ")
}

func TestNoLookup_FindsNothing(t *testing.T) {
	t.Parallel()

	label, pid := NoLookup(8080)
	assert.Empty(t, label)
	assert.Zero(t, pid)
}
