// Package disk reports free and total space on the filesystem holding a path.
//
// The implementation is per-platform: statfs on Linux, macOS and the BSDs,
// statvfs on Solaris/illumos, and GetDiskFreeSpaceEx on Windows. Platforms with
// no filesystem-statistics call at all (js/wasm, plan9) build against a stub
// that reports (0, 0).
//
// This doc comment lives in an untagged file on purpose: when it sat in the
// statfs implementation, every platform that did not build that file documented
// itself as nothing.
package disk
