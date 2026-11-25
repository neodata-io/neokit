// Package sqlite is the storage adapter: one Store over one database, which
// implements every repository interface the app services declare.
//
// One package and one type on purpose. Methods are grouped by concern into
// files (task.go, and the next aggregate's beside it), with this file owning
// the connection, the schema and the helpers they share. A second store type
// per aggregate would be a second abstraction over the same *sql.DB, and the
// interfaces in the app layer already keep each service from seeing more of the
// store than it uses.
package sqlite

import (
	"database/sql"
	"time"

	"github.com/neodata-io/neokit/sqlitex"
)

// Store is the service's database.
type Store struct {
	db *sql.DB
}

// Open opens the database at path and brings its schema up to date.
func Open(path string) (*Store, error) {
	db, err := sqlitex.Open(path, func(db *sql.DB) error { return sqlitex.Migrate(db, migrations) })
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// DB is the handle underneath, for the stores neokit ships (sessions, API
// tokens) that keep their own tables in this same database, and for a readiness
// check. Nothing in this service's own layers should need it.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database. Pushed onto the shutdown stack by the composition
// root, so it runs after the last request and the last job have finished.
func (s *Store) Close() error { return s.db.Close() }

// migrations is APPEND ONLY. A step's position is its version: editing or
// reordering one that has shipped silently diverges from every database already
// in the field. A fix is a new step at the end.
var migrations = []sqlitex.Migration{
	{Name: "tasks", Up: func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TABLE task (
				id           TEXT PRIMARY KEY,
				title        TEXT NOT NULL,
				done         INTEGER NOT NULL DEFAULT 0,
				version      INTEGER NOT NULL DEFAULT 1,
				created_at   TEXT NOT NULL,   -- RFC3339 UTC
				updated_at   TEXT NOT NULL,
				completed_at TEXT             -- NULL until done
			);
			CREATE INDEX idx_task_completed ON task(completed_at) WHERE completed_at IS NOT NULL;`)
		return err
	}},
}

// Instants are stored as RFC3339 in UTC: readable in a shell, and sortable as
// text, which is what lets the purge below be a ranged DELETE on a TEXT column.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func stampPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(*t)
}
