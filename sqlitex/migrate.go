// Package sqlitex provides SQLite operational helpers — a forward-only
// migration runner and a consistent snapshot — with no schema of its own.
package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migration is one forward-only schema step. Name is used only in logs and
// error messages; a step's position in the slice is its identity.
type Migration struct {
	Name string
	Up   func(*sql.Tx) error
}

// ErrSchemaFromTheFuture reports that the database was written by a build that
// knew more migrations than this one does.
var ErrSchemaFromTheFuture = errors.New("sqlitex: database schema is newer than this build")

// Migrate brings db up to len(migrations), running every step past the value
// in PRAGMA user_version — the free integer in every database file's header,
// so no bookkeeping table is needed. Each step runs in its own transaction
// with the version stamp inside it, so a crash can never leave a half-applied
// step recorded as done. At steady state this costs one integer read.
//
// The caller owns the slice and must treat it as APPEND ONLY: a step's index
// IS its version, so editing or reordering a shipped step silently diverges
// from every database already in the field. A fix is a new step.
//
// # Concurrent migrators
//
// Two processes starting together cannot both apply a step: the version is read
// again inside each step's transaction. On a handle from [Open] the second
// waits for the first, bounded by busy_timeout, then finds the step done and
// skips it. On a handle whose transactions are deferred it fails with
// SQLITE_BUSY instead — loudly, and without applying anything twice.
//
// # PRAGMAs inside a step
//
// SQLite silently ignores several PRAGMAs inside a transaction, including
// foreign_keys. A step following SQLite's own 12-step ALTER TABLE recipe, which
// requires PRAGMA foreign_keys=OFF around it, will therefore run with foreign
// keys still enforced. Do that work outside the runner.
func Migrate(db *sql.DB, migrations []Migration) error {
	return MigrateContext(context.Background(), db, migrations)
}

// MigrateContext is [Migrate] bounded by ctx, so a migration blocked on another
// process's lock can be abandoned instead of hanging startup indefinitely.
func MigrateContext(ctx context.Context, db *sql.DB, migrations []Migration) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	// A database ahead of this build is refused rather than ignored: the loop
	// below simply does not execute when version > len(migrations), which would
	// leave a rolled-back binary writing to a schema it does not understand.
	// Refusing to start is recoverable; operating on a future schema is not.
	if version > len(migrations) {
		return fmt.Errorf("%w: database is at version %d, this build knows %d migrations",
			ErrSchemaFromTheFuture, version, len(migrations))
	}

	for i := version; i < len(migrations); i++ {
		m := migrations[i]
		if err := applyStep(ctx, db, i, m); err != nil {
			// Both numbers, because they differ: the message counts steps from 1
			// for humans, while the index IS the version stamped in the file.
			return fmt.Errorf("migration %d (index %d, %q): %w", i+1, i, m.Name, err)
		}
	}
	return nil
}

func applyStep(ctx context.Context, db *sql.DB, i int, m Migration) error {
	if m.Up == nil {
		return errors.New("migration has a nil Up function")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	// Read again under the transaction: another process may have applied this
	// step since MigrateContext looked.
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if version > i {
		return nil
	}

	if err := m.Up(tx); err != nil {
		return err
	}
	// PRAGMA does not accept a bound parameter, and i+1 is an int we control.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
		return fmt.Errorf("stamp user_version: %w", err)
	}
	return tx.Commit()
}
