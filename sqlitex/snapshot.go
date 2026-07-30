package sqlitex

import (
	"context"
	"database/sql"
	"fmt"
)

// SnapshotTo writes a consistent, compacted copy of db to dst using SQLite's
// "VACUUM INTO". It reads under a single transaction, so a copy taken mid-write
// is never torn, and the output is a clean single file (no WAL/SHM) — unlike a
// naive copy of a WAL database, which can capture an inconsistent main file.
// The snapshot *is* a normal SQLite database: restoring it is just dropping it
// back in place.
func SnapshotTo(ctx context.Context, db *sql.DB, dst string) error {
	// VACUUM INTO must run in autocommit mode. The target is passed as a bind
	// parameter (SQLite accepts one here), sidestepping any path-quoting concerns.
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	return nil
}
