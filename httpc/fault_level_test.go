package httpc

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFault_LogLevel(t *testing.T) {
	warn := []Fault{FaultAuth, FaultConflict}
	for _, f := range warn {
		assert.Equal(t, slog.LevelWarn, f.LogLevel(), "%s is the admin's to fix", f)
	}
	quiet := []Fault{FaultNone, FaultNotFound, FaultRateLimited, FaultUnavailable, FaultUnsupported, FaultUnknown}
	for _, f := range quiet {
		assert.Equal(t, slog.LevelDebug, f.LogLevel(), "%s is background weather on a repeating job", f)
	}
}
