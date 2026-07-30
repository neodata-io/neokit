//go:build !unix

package disk

// Usage is a no-op on non-unix platforms, always returning (0, 0). The
// syscall.Statfs-backed implementation lives in diskfree_unix.go.
func Usage(path string) (free, total int64) { return 0, 0 }
