// Package sqlitex holds SQLite mechanism with no schema of its own.
package sqlitex

import (
	"database/sql"
	"fmt"
)

// Migration is one forward-only schema step. Name is used only in logs and
// error messages; a step's position in the slice is its identity.
type Migration struct {
	Name string
	Up   func(*sql.Tx) error
}

// Migrate brings db up to len(migrations), running every step past the value
// in PRAGMA user_version — the free integer in every database file's header,
// so no bookkeeping table is needed. Each step runs in its own transaction
// with the version stamp inside it, so a crash can never leave a half-applied
// step recorded as done. At steady state this costs one integer read.
//
// The caller owns the slice and must treat it as APPEND ONLY: a step's index
// IS its version, so editing or reordering a shipped step silently diverges
// from every database already in the field. A fix is a new step.
func Migrate(db *sql.DB, migrations []Migration) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	for i := version; i < len(migrations); i++ {
		m := migrations[i]
		if err := applyStep(db, i, m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", i+1, m.Name, err)
		}
	}
	return nil
}

func applyStep(db *sql.DB, i int, m Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	if err := m.Up(tx); err != nil {
		return err
	}
	// PRAGMA does not accept a bound parameter, and i+1 is an int we control.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
		return fmt.Errorf("stamp user_version: %w", err)
	}
	return tx.Commit()
}
