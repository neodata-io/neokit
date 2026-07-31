//go:build !plan9

package netx

import (
	"errors"
	"syscall"
)

// isAddrInUse reports whether err is the kernel's "address already in use".
//
// It lives in a build-tagged file because syscall.EADDRINUSE is not defined on
// plan9, which has no errno table — referencing it unguarded made the whole
// package (and every package importing it) fail to compile for that target.
func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
