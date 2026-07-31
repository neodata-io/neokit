//go:build !(linux || darwin || freebsd || android || aix || windows)

package disk

// Usage always reports (0, 0) on platforms with no filesystem-statistics call
// this package implements: OpenBSD, NetBSD, Solaris, illumos, js/wasm, wasip1
// and plan9.
//
// The tag must be the exact negation of the implemented set, not `!unix`: those
// are not complements, since OpenBSD, NetBSD, Solaris and illumos are all `unix`
// and would match the statfs file, which does not compile there, while matching
// no stub at all.
//
// Those platforms are stubbed rather than unsupported in principle —
// golang.org/x/sys/unix exposes statfs and statvfs for all of them — and are
// left out until there is a consumer, since a per-platform syscall wrapper no CI
// job exercises on real hardware is a liability.
func Usage(_ string) (free, total int64) { return 0, 0 }
