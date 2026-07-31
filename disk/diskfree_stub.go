//go:build !(linux || darwin || freebsd || android || aix || windows)

package disk

// Usage always reports (0, 0) on platforms with no filesystem-statistics call
// this package implements: OpenBSD, NetBSD, Solaris, illumos, js/wasm, wasip1
// and plan9.
//
// The tag is the exact negation of the implemented set rather than `!unix`.
// Those two are not complements: OpenBSD, NetBSD, Solaris and illumos are all
// `unix`, so under the old pair of tags they matched the statfs file — which
// does not compile there — and matched no stub at all.
//
// The BSD and Solaris members of this list are stubbed rather than unsupported
// in principle: golang.org/x/sys/unix exposes statfs (spelled F_bsize on
// OpenBSD) and statvfs (Solaris/illumos) for all of them. They are left out
// until there is a consumer on one, because a per-platform syscall wrapper that
// no CI job exercises on real hardware is a liability, not a feature.
func Usage(_ string) (free, total int64) { return 0, 0 }
