//go:build windows

package disk

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Usage reports free and total bytes on the volume holding the file at path.
// Returns (0, 0) when path is empty or the volume cannot be interrogated.
//
// GetDiskFreeSpaceEx rather than GetDiskFreeSpace: it reports the caller's
// quota-adjusted free space and does not overflow on volumes larger than 2 TB.
func Usage(path string) (free, total int64) {
	if !isMeasurable(path) {
		return 0, 0
	}
	dir, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		return 0, 0
	}
	// freeToCaller honours per-user quotas; totalFree is the volume-wide figure.
	// Reporting the caller's own budget is the useful answer for "can I still
	// write here", which is what every consumer of this asks.
	var freeToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &freeToCaller, &totalBytes, &totalFree); err != nil {
		return 0, 0
	}
	return int64(freeToCaller), int64(totalBytes)
}
