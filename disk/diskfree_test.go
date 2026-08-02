package disk_test

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/neodata-io/neokit/disk"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubbed reports whether this platform builds against the no-op
// implementation. Windows is *not* one: it has its own GetDiskFreeSpaceEx path,
// and an earlier version of this test wrongly expected zero there.
func stubbed() bool {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "android", "aix", "windows":
		return false
	default:
		return true
	}
}

func TestUsageReportsRealSpace(t *testing.T) {
	free, total, err := disk.Usage(filepath.Join(t.TempDir(), "some.db"))

	if stubbed() {
		require.ErrorIs(t, err, disk.ErrUnsupported)
		return
	}
	require.NoError(t, err)
	assert.Greater(t, total, int64(0), "a real directory must report nonzero total space")
	assert.GreaterOrEqual(t, free, int64(0))
}

// The failure that motivated the error return: zero free space and "I could not
// tell" are the same two numbers, and zero is the alarming one. A caller must be
// able to distinguish them.
func TestUsageReportsAnErrorRatherThanZero(t *testing.T) {
	_, _, err := disk.Usage("/this/path/almost-certainly/does-not-exist-anywhere/db.sqlite")
	require.Error(t, err, "an uninterrogable path must not read as a full disk")
}

// ":memory:" is a legal relative filename on unix, so without the name check
// filepath.Dir would reduce it to "." and Usage would confidently report the
// working directory's disk as if it were the database's.
func TestUsageRejectsPathsThatAreNotOnDisk(t *testing.T) {
	for _, path := range []string{"", ":memory:"} {
		free, total, err := disk.Usage(path)

		require.Error(t, err, "path %q", path)
		if !stubbed() {
			assert.True(t, errors.Is(err, disk.ErrNotOnDisk),
				"path %q: err = %v, want ErrNotOnDisk", path, err)
		}
		assert.Zero(t, free, "path %q", path)
		assert.Zero(t, total, "path %q", path)
	}
}
