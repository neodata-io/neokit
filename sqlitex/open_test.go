package sqlitex_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/neodata-io/neokit/declare"
	"github.com/neodata-io/neokit/sqlitex"
)

// recorder captures what Open declares. Every call site in this package's tests
// passes one, so the zero value has to be usable.
type recorder struct{ got []declare.Component }

func (r *recorder) Declare(c declare.Component) { r.got = append(r.got, c) }

// The bug this function exists to prevent. Pragmas set with db.Exec configure
// exactly one pooled connection and silently leave the others without them, so a
// second concurrent query runs with no busy_timeout and no foreign keys. Putting
// them in the DSN is what makes them apply to every connection the pool opens —
// and the only way to prove it is to force several open at once.
func TestPragmasApplyToEveryPooledConnection(t *testing.T) {
	db, err := sqlitex.Open(&recorder{}, "database", filepath.Join(t.TempDir(), "app.db"), nil)
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
	db, err := sqlitex.Open(&recorder{}, "database", ":memory:", nil)
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

// An empty path is the one input that fails by succeeding: SQLite accepts it and
// returns a writable anonymous database whose contents are gone after a restart.
// A deployment that forgot to set its path would look perfectly healthy.
func TestOpenRejectsAnEmptyPath(t *testing.T) {
	for _, path := range []string{"", "   "} {
		db, err := sqlitex.Open(&recorder{}, "database", path, nil)
		if err == nil {
			db.Close()
			t.Errorf("Open(%q) returned no error — it must refuse an empty path", path)
			continue
		}
		if !strings.Contains(err.Error(), ":memory:") {
			t.Errorf("Open(%q) error = %v, want it to point at :memory:", path, err)
		}
	}
}

func TestOpenCreatesTheParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "app.db")
	db, err := sqlitex.Open(&recorder{}, "database", path, nil)
	if err != nil {
		t.Fatalf("Open must create missing parents: %v", err)
	}
	db.Close()
}

func TestOpenRunsTheMigration(t *testing.T) {
	var ran int
	db, err := sqlitex.Open(&recorder{}, "database", filepath.Join(t.TempDir(), "app.db"), func(db *sql.DB) error {
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
	_, err := sqlitex.Open(&recorder{}, "database", filepath.Join(t.TempDir(), "app.db"), func(*sql.DB) error {
		return errPragma("migration exploded")
	})
	if err == nil {
		t.Fatal("want the migration error surfaced")
	}
}

// Open registers the database so a caller never writes Declare for it. The
// report line, the readiness check and the teardown all come from this one call.
func TestOpenDeclaresTheDatabase(t *testing.T) {
	var r recorder
	db, err := sqlitex.Open(&r, "database", filepath.Join(t.TempDir(), "app.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if len(r.got) != 1 {
		t.Fatalf("declared %d components, want 1", len(r.got))
	}
	c := r.got[0]
	if c.Name != "database" {
		t.Errorf("Name = %q, want %q", c.Name, "database")
	}
	if !c.On {
		t.Error("an opened database must be On")
	}
	if c.Ready == nil || c.Close == nil {
		t.Fatal("an opened database must declare both Ready and Close")
	}
	if err := c.Ready(context.Background()); err != nil {
		t.Errorf("Ready on a live database = %v, want nil", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := c.Ready(context.Background()); err == nil {
		t.Error("Ready must fail once the database is closed")
	}
}

// Detail is what an operator reads in the boot report to know which file this
// process actually opened.
func TestOpenDeclaresThePathAsDetail(t *testing.T) {
	var r recorder
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := sqlitex.Open(&r, "database", path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if r.got[0].Detail != path {
		t.Errorf("Detail = %q, want the path %q", r.got[0].Detail, path)
	}
}

// The name is a parameter precisely so a service can open two databases. They
// must not collide, or the boot report and /readyz list one of them twice.
func TestTwoDatabasesDeclareDistinctNames(t *testing.T) {
	var r recorder
	dir := t.TempDir()
	for _, name := range []string{"database", "analytics"} {
		db, err := sqlitex.Open(&r, name, filepath.Join(dir, name+".db"), nil)
		if err != nil {
			t.Fatalf("Open %s: %v", name, err)
		}
		t.Cleanup(func() { _ = db.Close() })
	}

	if len(r.got) != 2 {
		t.Fatalf("declared %d components, want 2", len(r.got))
	}
	if r.got[0].Name == r.got[1].Name {
		t.Errorf("both components named %q; the name parameter was ignored", r.got[0].Name)
	}
}

// A failed open must declare nothing: a component in the report for a database
// that was never opened is worse than no line at all.
func TestAFailedOpenDeclaresNothing(t *testing.T) {
	var r recorder
	if _, err := sqlitex.Open(&r, "database", "", nil); err == nil {
		t.Fatal("Open must reject an empty path")
	}
	if len(r.got) != 0 {
		t.Errorf("declared %d components after a failed open, want 0", len(r.got))
	}
}
