package neokit_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	neokit "github.com/neodata-io/neokit"
	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/backup"
	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/oidcauth/fiberauth"
)

func newKit(t *testing.T) *neokit.App {
	t.Helper()
	a, err := neokit.New(app.Options{
		Name: "testapp",
		Base: config.Base{Port: 0, LogLevel: "error", LogFormat: "json"},
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("neokit.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// One call: the handle comes back and its Close is registered for teardown.
func TestDatabaseRegistersItsTeardown(t *testing.T) {
	a := newKit(t)
	before := a.Shutdown.Len()

	db, err := a.Database(filepath.Join(t.TempDir(), "app.db"), nil)
	if err != nil {
		t.Fatalf("Database: %v", err)
	}

	if err := db.PingContext(context.Background()); err != nil {
		t.Errorf("the returned handle must work: %v", err)
	}
	if got := a.Shutdown.Len(); got != before+1 {
		t.Errorf("Shutdown grew by %d, want 1 — Database must register its own Close", got-before)
	}
}

// Login mounts the gate through the embedded app.
func TestLoginMountsTheGate(t *testing.T) {
	a := newKit(t)
	gate := a.Login(fiberauth.Options{CookiePrefix: "testapp"})
	if gate == nil {
		t.Fatal("Login returned nil")
	}
	if gate.Enabled() {
		t.Error("a gate with no Provider must report disabled")
	}
}

// noopSnap satisfies backup.Snapshotter without touching a real database —
// Backups starts Run's schedule immediately (unlike the old declare-based path,
// which deferred every declared Run until app.Run()), so a nil Snapshotter here
// would panic the very goroutine this test is trying to prove is usable.
type noopSnap struct{}

func (noopSnap) SnapshotTo(context.Context, string) error { return nil }

// Backups returns a usable service wired against the embedded app's context.
func TestBackupsReturnsAUsableService(t *testing.T) {
	a := newKit(t)
	svc := a.Backups(noopSnap{}, backup.Options{Dir: t.TempDir(), Retention: 3})
	if svc == nil {
		t.Fatal("Backups returned nil")
	}
	if _, err := svc.List(context.Background()); err != nil {
		t.Errorf("List: %v", err)
	}
}

// Embedding is the contract: everything app.App has is reachable unchanged.
func TestEmbeddingExposesApp(t *testing.T) {
	a := newKit(t)
	before := a.Shutdown.Len()
	a.Shutdown.Push("plugins", func(context.Context) error { return nil })
	if a.HTTP == nil {
		t.Error("embedded HTTP not reachable")
	}
	if got := a.Shutdown.Len(); got != before+1 {
		t.Error("embedded Shutdown did not register the pushed step")
	}
}
