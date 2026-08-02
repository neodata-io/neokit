// Package disk reports free and total space on the filesystem holding a path.
//
// The implementation is per-platform: statfs on Linux, macOS and the BSDs,
// statvfs on Solaris/illumos, and GetDiskFreeSpaceEx on Windows. Platforms with
// no filesystem-statistics call at all (js/wasm, plan9) build against a stub
// that returns [ErrUnsupported].
//
// This doc comment lives in an untagged file on purpose: when it sat in the
// statfs implementation, every platform that did not build that file documented
// itself as nothing.
package disk

import "errors"

// ErrUnsupported is returned by [Usage] on a platform with no filesystem
// statistics call implemented here.
var ErrUnsupported = errors.New("disk: filesystem statistics are unsupported on this platform")

// ErrNotOnDisk is returned by [Usage] for a path that names no filesystem
// location — an empty string, or SQLite's ":memory:".
var ErrNotOnDisk = errors.New("disk: path does not name a filesystem location")
