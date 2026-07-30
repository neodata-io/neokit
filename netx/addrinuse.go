// Package netx holds small, dependency-free network helpers.
package netx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// AddrInUseHint turns the kernel's terse "bind: address already in use" into a
// message that names the culprit process and the command to clear it. The usual
// cause is a stale instance of the same binary still listening from a previous
// run, and "which process do I kill" shouldn't cost a manual round-trip through
// lsof. envVar is the setting that would move this listener elsewhere (e.g. PORT).
//
// Any error that isn't EADDRINUSE is returned untouched, so a caller can wrap
// every listen error through this unconditionally.
func AddrInUseHint(err error, port int, envVar string) error {
	if !errors.Is(err, syscall.EADDRINUSE) {
		return err
	}
	if holder, pid := portLookup(port); holder != "" {
		return fmt.Errorf("port %d is already in use by %s — stop it with `kill %d`, or set %s to a free port", port, holder, pid, envVar)
	}
	return fmt.Errorf("port %d is already in use — stop whatever is listening on it, or set %s to a free port", port, envVar)
}

// portLookup resolves the process currently listening on port, in the shape
// portHolder returns. It is a package variable rather than a direct call so
// tests can substitute a stub instead of shelling out to lsof.
var portLookup = portHolder

// portHolder best-effort identifies the process listening on port, returning a
// human label ("api (pid 5201)") and the pid. It shells out to lsof, which is
// present on the macOS/Linux dev machines where a port clash actually happens;
// when it's missing — a slim container, say — it simply returns nothing and the
// caller falls back to the generic message. A diagnostic is never worth failing
// startup over, so every error here is swallowed.
func portHolder(port int) (string, int) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return "", 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// -F pc gives a stable machine-readable form: one field per line, "p" for the
	// pid and "c" for the command name.
	out, err := exec.CommandContext(ctx, lsof,
		"-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-F", "pc").Output()
	if err != nil {
		return "", 0
	}

	var pid int
	var name string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			if _, err := fmt.Sscanf(line[1:], "%d", &pid); err != nil {
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
