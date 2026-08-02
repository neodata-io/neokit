//go:build linux || darwin || freebsd || android || aix

package disk

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// Usage reports free and total bytes on the filesystem holding the file at
// path. On error it returns (0, 0, err): a caller that ignores the error reads
// zero free, which is the alarming direction, so the error is not optional.
//
// Usage calls syscall.Statfs, which can block indefinitely if path resolves to
// an unreachable network mount. Only point this at local storage.
//
// The build tag lists platforms individually rather than saying `unix`, because
// `unix` is not the same set as "has syscall.Statfs_t with a Bsize field":
// OpenBSD spells that field F_bsize, and NetBSD, Solaris and illumos do not
// export Statfs at all. Tagged `unix`, this file failed to compile on four
// targets — a break no CI job built.
func Usage(path string) (free, total int64, err error) {
	if !isMeasurable(path) {
		return 0, 0, fmt.Errorf("%w: %q", ErrNotOnDisk, path)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &st); err != nil {
		return 0, 0, fmt.Errorf("disk: statfs %q: %w", filepath.Dir(path), err)
	}
	// Bsize is int64 on Linux, uint32 on macOS; widen both to uint64 so the
	// arithmetic compiles on every target above.
	bsize := uint64(st.Bsize)
	return int64(uint64(st.Bavail) * bsize), int64(uint64(st.Blocks) * bsize), nil
}
