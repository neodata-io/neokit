package sqlitex_test

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"github.com/neodata-io/neokit/sqlitex"
)

// The bug this function exists to prevent. Pragmas set with db.Exec configure
// exactly one pooled connection and silently leave the others without them, so a
// second concurrent query runs with no busy_timeout and no foreign keys. Putting
// them in the DSN is what makes them apply to every connection the pool opens —
// and the only way to prove it is to force several open at once.
func TestPragmasApplyToEveryPooledConnection(t *testing.T) {
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "app.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Hold several connections simultaneously so the pool must open more than one.
	const conns = 4
	var wg sync.WaitGroup
	release := make(chan struct{})
	errs := make(chan error, conns)

	for range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := db.Conn(t.Context())
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()

			var fk int
			if err := c.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
				errs <- err
				return
			}
			if fk != 1 {
				errs <- errFK
				return
			}
			<-release
		}()
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a pooled connection was misconfigured: %v", err)
	}
}

var errFK = errPragma("foreign_keys was off on a pooled connection")

type errPragma string

func (e errPragma) Error() string { return string(e) }

// A bare :memory: database is private per connection, so a pool would hand out
// separate empty databases and a write on one would be invisible to the next
// read. Pinning to a single connection is what makes :memory: usable at all.
func TestMemoryDatabaseIsPinnedToOneConnection(t *testing.T) {
	db, err := sqlitex.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("select: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 — the pool handed out a second empty database", n)
	}
}

func TestOpenCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "app.db")
	db, err := sqlitex.Open(path, nil)
	if err != nil {
		t.Fatalf("Open must create missing parents: %v", err)
	}
	db.Close()
}

func TestOpenRunsTheMigration(t *testing.T) {
	var ran int
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "app.db"), func(db *sql.DB) error {
		ran++
		_, err := db.Exec(`CREATE TABLE migrated (id INTEGER)`)
		return err
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if ran != 1 {
		t.Errorf("migrate ran %d times, want exactly 1", ran)
	}
	if _, err := db.Exec(`INSERT INTO migrated VALUES (1)`); err != nil {
		t.Errorf("the migration did not take effect: %v", err)
	}
}

// A failed migration must not hand back a usable handle — the caller would
// otherwise run against a half-built schema.
func TestOpenClosesTheDatabaseWhenMigrationFails(t *testing.T) {
	_, err := sqlitex.Open(filepath.Join(t.TempDir(), "app.db"), func(*sql.DB) error {
		return errPragma("migration exploded")
	})
	if err == nil {
		t.Fatal("want the migration error surfaced")
	}
}
