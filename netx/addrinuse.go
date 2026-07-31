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

// NoLookup is a Lookup that never identifies anything, and is [AddrInUseHint]'s
// default: the generic message, with no subprocess execution.
func NoLookup(int) (string, int) { return "", 0 }

// AddrInUseHint turns the kernel's terse "bind: address already in use" into a
// message naming the port and the setting that would move this listener
// elsewhere (envVar, e.g. PORT). Pass [LsofLookup] to also name the process
// holding the port and the command to kill it.
//
// Any error that isn't EADDRINUSE is returned untouched, so every listen error
// can be routed through this unconditionally. The result always wraps the
// original, so errors.Is(…, syscall.EADDRINUSE) still holds downstream.
//
// lookup is optional; only the first is used, and a nil one selects the default.
func AddrInUseHint(err error, port int, envVar string, lookup ...Lookup) error {
	if !isAddrInUse(err) {
		return err
	}
	find := Lookup(NoLookup)
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

// lsofTimeout bounds the diagnostic: a hint is never worth delaying a startup
// failure over.
const lsofTimeout = 2 * time.Second

// LsofLookup best-effort identifies the process listening on port by executing
// lsof, returning a human label ("api (pid 5201)") and the pid. Pass it to
// [AddrInUseHint] to opt into naming the offending process — worth it on a
// developer machine, where a stale instance of the same binary is the usual
// cause of a port clash.
//
// Being an exec, it does not work everywhere: lsof is absent from slim and
// distroless images, and on a hardened host it must run as root to see another
// user's sockets. Every error is swallowed and returns ("", 0), so the caller
// falls back to the generic message and a missing lsof costs nothing.
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
