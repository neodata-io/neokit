// Package neokit is the batteries-included layer: one constructor, then one
// method per feature — a.Database, a.Login, a.Backups, the notify senders.
// Calling a method is enabling the feature; each returns what it builds, so a
// missing dependency is a compile error, never a runtime lookup.
//
//	a, err := neokit.New(app.Options{Name: "myapp", Base: cfg.Base})
//	if err != nil { return err }
//	defer a.Close()
//
//	db, err := a.Database(cfg.DatabasePath, migrate)
//	gate := a.Login(fiberauth.Options{Sessions: store})
//
//	return a.Run()
//
// Importing this package compiles every feature it fronts — SQLite, OIDC, the
// notify backends. A service that wants less imports [app] and the feature
// packages it actually uses; they compose identically, this is only the
// convenient spelling.
package neokit

import (
	"database/sql"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/backup"
	"github.com/neodata-io/neokit/notify"
	"github.com/neodata-io/neokit/oidcauth/fiberauth"
	"github.com/neodata-io/neokit/sqlitex"
)

// App is [app.App] plus one method per neokit feature. Everything the embedded
// app exports — HTTP, Shutdown, Run, ClosesOnShutdown — is reachable unchanged.
type App struct{ *app.App }

// New builds the application. See [app.New] for the boot sequence.
func New(o app.Options) (*App, error) {
	a, err := app.New(o)
	if err != nil {
		return nil, err
	}
	return &App{App: a}, nil
}

// Database opens the service's SQLite database and registers it: boot report,
// readiness check, shutdown step. See [sqlitex.Open].
func (a *App) Database(path string, migrate func(*sql.DB) error) (*sql.DB, error) {
	return sqlitex.Open(a.App, path, migrate)
}

// Login builds the OIDC login gate, mounts its routes and registers it. See
// [fiberauth.New].
func (a *App) Login(o fiberauth.Options) *fiberauth.Gate {
	return fiberauth.New(a.App, o)
}

// Backups wires scheduled database backups and registers them. See [backup.New].
func (a *App) Backups(s backup.Snapshotter, o backup.Options) *backup.Service {
	return backup.New(a.App, s, o)
}

// Webhook builds a signed-webhook notification sender and registers it.
func (a *App) Webhook(url, secret string, o notify.Options) *notify.Webhook {
	return notify.NewWebhook(a.App, url, secret, o)
}

// Ntfy builds an ntfy notification sender and registers it.
func (a *App) Ntfy(topicURL, token string, o notify.Options) *notify.Ntfy {
	return notify.NewNtfy(a.App, topicURL, token, o)
}

// Apprise builds an Apprise notification sender and registers it.
func (a *App) Apprise(url string, o notify.Options) *notify.Apprise {
	return notify.NewApprise(a.App, url, o)
}
