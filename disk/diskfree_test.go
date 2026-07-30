package disk_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/neodata-io/neokit/disk"

	"github.com/stretchr/testify/assert"
)

func TestUsage_RealDirectoryReportsNonZeroTotal(t *testing.T) {
	dir := t.TempDir()
	free, total := disk.Usage(filepath.Join(dir, "some.db"))

	switch runtime.GOOS {
	case "windows", "plan9", "js":
		// The build-tagged fallback on non-unix platforms is a documented no-op.
		assert.Zero(t, total)
	default:
		assert.Greater(t, total, int64(0), "a real, existing directory must report nonzero total space")
		assert.GreaterOrEqual(t, free, int64(0))
	}
}

func TestUsage_NonexistentPathReturnsZero(t *testing.T) {
	free, total := disk.Usage("/this/path/almost-certainly/does-not-exist-anywhere/db.sqlite")
	assert.Zero(t, free)
	assert.Zero(t, total)
}

func TestUsage_UnusablePathReturnsZero(t *testing.T) {
	for _, path := range []string{"", ":memory:"} {
		free, total := disk.Usage(path)
		assert.Zero(t, free, "path %q", path)
		assert.Zero(t, total, "path %q", path)
	}
}
