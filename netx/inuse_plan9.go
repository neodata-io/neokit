//go:build plan9

package netx

import "strings"

// isAddrInUse reports whether err is plan9's "address already in use".
//
// Plan 9 has no errno table, so there is no sentinel to compare against and
// errors.Is cannot help: the kernel returns the condition as message text. A
// substring match is the only test available, and it is confined to this file
// so no other platform inherits string matching.
func isAddrInUse(err error) bool {
	return err != nil && strings.Contains(err.Error(), "address in use")
}
