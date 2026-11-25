package sqlitex

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// SnapshotTo writes a consistent, compacted copy of db to dst using SQLite's
// "VACUUM INTO". It reads under a single transaction, so a copy taken mid-write
// is never torn, and the output is a clean single file (no WAL/SHM) — unlike a
// naive copy of a WAL database, which can capture an inconsistent main file.
// The snapshot *is* a normal SQLite database: restoring it is just dropping it
// back in place.
//
// dst must not exist. It is created 0600 and removed again if the snapshot
// fails. It is not synced to disk; a caller that needs that does it.
func SnapshotTo(ctx context.Context, db *sql.DB, dst string) error {
	// Created here rather than by SQLite, which would make it 0644. Exclusive,
	// so a failure can remove it knowing it is ours. VACUUM INTO accepts an
	// empty target.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	// VACUUM INTO must run in autocommit mode. The target is passed as a bind
	// parameter (SQLite accepts one here), sidestepping any path-quoting concerns.
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("snapshot database: %w", err)
	}
	return nil
}
