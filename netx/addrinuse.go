// Package netx holds small network helpers.
package netx

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Lookup identifies the process listening on a port, returning a human label
// such as "api (pid 5201)" and the pid. A lookup that finds nothing returns
// ("", 0); it must never fail the caller, since this is only ever a diagnostic.
type Lookup func(port int) (label string, pid int)

// NoLookup is a Lookup that never identifies anything. Pass it to
// [AddrInUseHint] to get the generic message with no subprocess execution —
// the right choice for a hardened or minimal deployment, where [LsofLookup]
// would find nothing anyway. See [AddrInUseHint] on why it is not the default.
func NoLookup(int) (string, int) { return "", 0 }

// AddrInUseHint turns the kernel's terse "bind: address already in use" into a
// message that names the culprit process and the command to clear it. The usual
// cause is a stale instance of the same binary still listening from a previous
// run, and "which process do I kill" shouldn't cost a manual round-trip through
// lsof. envVar is the setting that would move this listener elsewhere (e.g. PORT).
//
// Any error that isn't EADDRINUSE is returned untouched, so a caller can wrap
// every listen error through this unconditionally. The returned error always
// wraps the original, so errors.Is(…, syscall.EADDRINUSE) still holds
// downstream — which is exactly what "wrap unconditionally" has to mean, and
// what an earlier version quietly broke by formatting without %w.
//
// lookup is optional and defaults to [LsofLookup], which executes lsof. That
// default is a deliberate trade and worth stating plainly: a library reaching
// for an external binary is normally the wrong instinct, and it is kept because
// naming the offending process is the entire value of this function on the
// developer machines where a port clash actually happens. Pass [NoLookup] to
// opt out, or any Lookup of your own. Only the first is used.
func AddrInUseHint(err error, port int, envVar string, lookup ...Lookup) error {
	if !isAddrInUse(err) {
		return err
	}
	find := Lookup(LsofLookup)
	if len(lookup) > 0 && lookup[0] != nil {
		find = lookup[0]
	}
	if holder, pid := find(port); holder != "" {
		return fmt.Errorf("port %d is already in use by %s — stop it with `kill %d`, or set %s to a free port: %w",
			port, holder, pid, envVar, err)
	}
	return fmt.Errorf("port %d is already in use — stop whatever is listening on it, or set %s to a free port: %w",
		port, envVar, err)
}

// lsofTimeout bounds the diagnostic. A hint is never worth delaying a startup
// failure over, and this runs on a path where the process is already failing.
const lsofTimeout = 2 * time.Second

// LsofLookup best-effort identifies the process listening on port by executing
// lsof, returning a human label ("api (pid 5201)") and the pid.
//
// It is the default [AddrInUseHint] lookup. Being an exec, it is also the part
// of this package that will not work everywhere: lsof is absent from slim and
// distroless containers, and on a hardened host it must run as root to see
// another user's sockets. Both cases simply return ("", 0) and the caller falls
// back to the generic message, so a missing lsof costs nothing. Every error is
// swallowed for the same reason.
func LsofLookup(port int) (string, int) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return "", 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()

	// -F pc gives a stable machine-readable form: one field per line, "p" for the
	// pid and "c" for the command name.
	out, err := exec.CommandContext(ctx, lsof,
		"-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-F", "pc").Output()
	if err != nil {
		return "", 0
	}

	var pid int
	var name string
	for line := range strings.SplitSeq(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			if pid, err = strconv.Atoi(line[1:]); err != nil {
				return "", 0
			}
		case 'c':
			name = line[1:]
		}
		// The first pid/command pair is enough — a listening socket has one owner,
		// and forked workers would only add noise.
		if pid != 0 && name != "" {
			return fmt.Sprintf("%s (pid %d)", name, pid), pid
		}
	}
	return "", 0
}
