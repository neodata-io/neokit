package logx

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureSetup runs Setup with os.Stderr redirected to a pipe and returns
// everything the resulting logger wrote while fn ran. Setup binds os.Stderr at
// call time, so the swap has to bracket the call itself. A pipe is also not a
// TTY, which is exactly the production shape: format "auto" resolves to JSON.
func captureSetup(t *testing.T, level, format string, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	origStderr, origLogger := os.Stderr, slog.Default()
	t.Cleanup(func() { os.Stderr = origStderr; slog.SetDefault(origLogger) })

	os.Stderr = w
	Setup("neogate", level, format, "v1.12.2")
	fn()
	require.NoError(t, w.Close())

	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	return buf.String()
}

// records parses the captured stream into one map per JSON line. Duplicate keys
// collapse last-wins here, exactly as a log aggregator would collapse them —
// which is why the duplicate-key test below asserts on the raw bytes instead.
func records(t *testing.T, out string) []map[string]any {
	t.Helper()

	var recs []map[string]any
	dec := json.NewDecoder(bytes.NewReader([]byte(out)))
	for dec.More() {
		var m map[string]any
		require.NoError(t, dec.Decode(&m))
		recs = append(recs, m)
	}
	return recs
}

func find(recs []map[string]any, msg string) map[string]any {
	for _, r := range recs {
		if r["msg"] == msg {
			return r
		}
	}
	return nil
}

// A DEBUG record must not survive LOG_LEVEL=info. This is the regression guard
// for a production report of debug lines under an info deployment: it pins that
// the level really is enforced by the handler, so the next such report is an
// environment problem and not a logging-pipeline one.
func TestSetupInfoSuppressesDebug(t *testing.T) {
	out := captureSetup(t, "info", "json", func() {
		slog.Debug("health check failed", "service", "fluvius")
		slog.Info("kept")
	})

	assert.NotContains(t, out, "health check failed")
	assert.Contains(t, out, "kept")
}

func TestSetupDebugEmitsDebug(t *testing.T) {
	out := captureSetup(t, "debug", "json", func() {
		slog.Debug("health check failed", "service", "fluvius")
	})

	assert.Contains(t, out, "health check failed")
}

// An unparseable level falls back to info rather than to debug — a typo in the
// deployment must not silently turn the logs verbose. The complaint about it
// must also go through the configured handler: reported before SetDefault it
// came out as bootstrap plain text, making the one line that explains the
// unexpected level the one line a JSON pipeline drops.
func TestSetupInvalidLevelFallsBackToInfo(t *testing.T) {
	out := captureSetup(t, "verbose", "json", func() {
		slog.Debug("suppressed")
		slog.Info("kept")
	})

	assert.NotContains(t, out, "suppressed")
	assert.Contains(t, out, "kept")

	rec := find(records(t, out), "invalid LOG_LEVEL, defaulting to info")
	require.NotNil(t, rec, "the warning must be structured like every other line: %s", out)
	assert.Equal(t, "verbose", rec["value"])
	assert.Equal(t, "neogate", rec["app"])
	assert.Equal(t, "INFO", find(records(t, out), "logging configured")["level_configured"])
}

// Setup announces the level it resolved. Without this the effective level is
// unobservable in a container: printBanner renders only on a TTY, so a
// misconfigured deployment can only be diagnosed by reading source.
func TestSetupAnnouncesEffectiveLevel(t *testing.T) {
	out := captureSetup(t, "info", "json", func() {})

	rec := find(records(t, out), "logging configured")
	require.NotNil(t, rec, "Setup must announce the level it resolved: %s", out)
	assert.Equal(t, "INFO", rec["level_configured"])
	assert.Equal(t, "json", rec["format"])
}

// The announcement respects the threshold it is announcing: at warn nobody
// asked for an info line, and emitting one anyway would be the logger
// disobeying its own configuration.
func TestSetupAnnouncementRespectsThreshold(t *testing.T) {
	out := captureSetup(t, "warn", "json", func() {})

	assert.NotContains(t, out, "logging configured")
}

// The root logger must not claim "service": the SDK hands that key to plugins
// (neogate.Log stamps service=<integration>), and every host-side line about a
// plugin uses it the same way. A root attr under the same name puts two
// "service" keys in one JSON record, and an aggregator keeps whichever it sees
// last — so the process identity and the integration name silently overwrite
// each other.
func TestRootLoggerDoesNotShadowPluginServiceKey(t *testing.T) {
	out := captureSetup(t, "info", "json", func() {
		slog.Default().With(slog.String("service", "fluvius")).Info("health check failed")
	})

	rec := find(records(t, out), "health check failed")
	require.NotNil(t, rec)
	assert.Equal(t, "fluvius", rec["service"])

	// Asserted on the raw line, not the parsed map: encoding/json already
	// collapsed any duplicate to last-wins, which is the very failure being
	// tested for.
	line := rawLine(t, out, "health check failed")
	assert.Equal(t, 1, bytes.Count([]byte(line), []byte(`"service":`)),
		"exactly one service key per record: %s", line)
}

// rawLine returns the unparsed JSON line whose msg is msg.
func rawLine(t *testing.T, out, msg string) string {
	t.Helper()

	for _, line := range bytes.Split([]byte(out), []byte("\n")) {
		if bytes.Contains(line, []byte(`"msg":"`+msg+`"`)) {
			return string(line)
		}
	}
	t.Fatalf("no record with msg %q in: %s", msg, out)
	return ""
}

// Process identity still ships on every line, just under a key that is nobody
// else's.
func TestRootLoggerStampsAppAndVersion(t *testing.T) {
	out := captureSetup(t, "info", "json", func() {
		slog.Info("anything")
	})

	rec := find(records(t, out), "anything")
	require.NotNil(t, rec)
	assert.Equal(t, "neogate", rec["app"])
	assert.Equal(t, "v1.12.2", rec["version"])
}
