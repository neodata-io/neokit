package app_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
)

// appWithLog builds an app whose logger writes somewhere a test can read.
func appWithLog(t *testing.T, into *bytes.Buffer) *app.App {
	t.Helper()
	a, err := app.New(app.Options{
		Name: "testapp",
		Base: config.Base{Port: 0, LogLevel: "debug", LogFormat: "json"},
		Log:  slog.New(slog.NewJSONHandler(into, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// Name is the one field with no sensible default: it labels the report line, the
// readiness check and the shutdown step. New already refuses an empty
// Options.Name for the same reason, so Declare refuses an empty Component.Name.
func TestDeclareRequiresAName(t *testing.T) {
	a := newApp(t)

	defer func() {
		if recover() == nil {
			t.Error("Declare must reject a component with no Name")
		}
	}()
	a.Declare(app.Component{On: true, Detail: "anonymous"})
}

// A repeated name yields two report lines, two readiness checks and two shutdown
// steps — always a copy-paste mistake, never intent.
func TestDeclareWarnsOnADuplicateName(t *testing.T) {
	var logged bytes.Buffer
	a := appWithLog(t, &logged)

	a.Declare(app.Component{Name: "database", On: true, Detail: "first"})
	a.Declare(app.Component{Name: "database", On: true, Detail: "second"})

	if !strings.Contains(logged.String(), "already declared") {
		t.Errorf("want a warning naming the duplicate; got:\n%s", logged.String())
	}
}

// The names neokit declares for itself are part of the same namespace, so
// colliding with one is worth the same warning.
func TestDeclareWarnsWhenItCollidesWithABuiltIn(t *testing.T) {
	var logged bytes.Buffer
	a := appWithLog(t, &logged)

	a.Declare(app.Component{Name: "tracing", On: true, Detail: "mine, not neokit's"})

	if !strings.Contains(logged.String(), "already declared") {
		t.Errorf("want a warning for colliding with neokit's own component; got:\n%s", logged.String())
	}
}
