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
	"github.com/neodata-io/neokit/sqlitex"
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

func has(a *neokit.App, name string) bool {
	for _, c := range a.Components() {
		if c.Name == name {
			return true
		}
	}
	return false
}

// One call: the handle comes back, and the component is registered.
func TestDatabaseRegistersAndReturns(t *testing.T) {
	a := newKit(t)
	db, err := a.Database(filepath.Join(t.TempDir(), "app.db"), nil)
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.PingContext(context.Background()); err != nil {
		t.Errorf("the returned handle must work: %v", err)
	}
	if !has(a, sqlitex.ComponentName) {
		t.Errorf("%q missing from Components()", sqlitex.ComponentName)
	}
}

// Login mounts and declares through the embedded app.
func TestLoginRegistersTheGate(t *testing.T) {
	a := newKit(t)
	gate := a.Login(fiberauth.Options{CookiePrefix: "testapp"})
	if gate == nil {
		t.Fatal("Login returned nil")
	}
	if !has(a, "login") {
		t.Error("login missing from Components()")
	}
}

// Backups declares through the embedded app.
func TestBackupsRegisters(t *testing.T) {
	a := newKit(t)
	svc := a.Backups(nil, backup.Options{Dir: t.TempDir(), Retention: 3})
	if svc == nil {
		t.Fatal("Backups returned nil")
	}
	if !has(a, "backups") {
		t.Error("backups missing from Components()")
	}
}

// Embedding is the contract: everything app.App has is reachable unchanged.
func TestEmbeddingExposesApp(t *testing.T) {
	a := newKit(t)
	a.ClosesOnShutdown("plugins", "3 loaded", func(context.Context) error { return nil })
	if a.HTTP == nil {
		t.Error("embedded HTTP not reachable")
	}
	if !has(a, "plugins") {
		t.Error("embedded ClosesOnShutdown did not register")
	}
}
