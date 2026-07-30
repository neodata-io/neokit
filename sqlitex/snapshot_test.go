package sqlitex_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/neodata-io/neokit/sqlitex"
	_ "modernc.org/sqlite"
)

func TestSnapshotToProducesAStandaloneDatabaseWithThePointInTimeRows(t *testing.T) {
	ctx := context.Background()
	src := openMem(t)
	if _, err := src.Exec("CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := src.Exec("INSERT INTO items (id, name) VALUES (1, 'a'), (2, 'b')"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "snapshot.db")
	if err := sqlitex.SnapshotTo(ctx, src, dst); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	// Mutate the source after the snapshot: a copy taken mid-write (or here,
	// just-before-a-write) must not see it — that's the transactional-
	// consistency guarantee VACUUM INTO exists for.
	if _, err := src.Exec("INSERT INTO items (id, name) VALUES (3, 'c')"); err != nil {
		t.Fatalf("insert after snapshot: %v", err)
	}

	// The snapshot must be a plain, standalone SQLite file — openable on its
	// own, with no -wal/-shm sidecars.
	if fi, err := os.Stat(dst); err != nil || fi.Size() == 0 {
		t.Fatalf("snapshot file missing or empty: %v", err)
	}
	if _, err := os.Stat(dst + "-wal"); !os.IsNotExist(err) {
		t.Errorf("snapshot must not have a WAL sidecar, stat err = %v", err)
	}

	snap, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snap.Close()

	var count int
	if err := snap.QueryRow("SELECT COUNT(*) FROM items").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("snapshot row count = %d, want 2 (must reflect the state at snapshot time, not after)", count)
	}

	var name string
	if err := snap.QueryRow("SELECT name FROM items WHERE id = 1").Scan(&name); err != nil {
		t.Fatalf("select: %v", err)
	}
	if name != "a" {
		t.Errorf("name = %q, want %q", name, "a")
	}
}
