//go:build unix

// Package disk reports free and total space on the filesystem holding a file.
// A build-tagged fallback keeps the package building — as a documented no-op —
// on platforms without syscall.Statfs.
package disk

import (
	"path/filepath"
	"syscall"
)

// Usage reports free and total bytes on the filesystem holding the file at
// path. Returns (0, 0) when path is unusable — empty, or ":memory:", SQLite's
// marker for a database that never touches disk.
//
// Usage calls syscall.Statfs, which can block indefinitely if path resolves to
// an unreachable network mount. Only point this at local storage.
func Usage(path string) (free, total int64) {
	if path == "" || path == ":memory:" {
		return 0, 0
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &st); err != nil {
		return 0, 0
	}
	// Bsize is int64 on Linux, uint32 on macOS; widen both to uint64 so the
	// arithmetic compiles on every unix target.
	bsize := uint64(st.Bsize)
	return int64(uint64(st.Bavail) * bsize), int64(uint64(st.Blocks) * bsize)
}
