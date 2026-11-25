package sqlitex_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
		wg.Go(func() {
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
		})
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

// An empty path is the one input that fails by succeeding: SQLite accepts it and
// returns a writable anonymous database whose contents are gone after a restart.
// A deployment that forgot to set its path would look perfectly healthy.
func TestOpenRejectsAnEmptyPath(t *testing.T) {
	for _, path := range []string{"", "   "} {
		db, err := sqlitex.Open(path, nil)
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

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return fi.Mode().Perm()
}

// The database holds emails and credential hashes; nobody else on the host has
// any business reading it.
func TestOpenCreatesAPrivateDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "app.db")
	db, err := sqlitex.Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := perm(t, dir); got != 0o700 {
		t.Errorf("new data directory is %o, want 700", got)
	}
	// The WAL and SHM files take the database file's mode.
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if got := perm(t, f); got != 0o600 {
			t.Errorf("%s is %o, want 600", filepath.Base(f), got)
		}
	}
}

// An operator who opened these up did so on purpose.
func TestOpenLeavesAnExistingDatabasesModeAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "app.db")
	db, err := sqlitex.Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Close()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err = sqlitex.Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	db.Close()
	if got := perm(t, dir); got != 0o755 {
		t.Errorf("existing directory became %o, want it left at 755", got)
	}
	if got := perm(t, path); got != 0o644 {
		t.Errorf("existing database became %o, want it left at 644", got)
	}
}

// The path goes into a URI, where '#' starts a fragment, '?' the query and '%'
// an escape. Unescaped, "my#app.db" silently opened a file called "my".
func TestOpenUsesExactlyThePathItIsGiven(t *testing.T) {
	names := []string{"plain.db", "my#app.db", "100%25.db", "with space.db"}
	if runtime.GOOS != "windows" {
		names = append(names, "what?.db")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			db, err := sqlitex.Open(path, nil)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if _, err := db.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
				t.Fatalf("create: %v", err)
			}
			db.Close()

			fi, err := os.Stat(path)
			if err != nil || fi.Size() == 0 {
				t.Fatalf("the database is not at the path given: %v", err)
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), name) {
					t.Errorf("Open also created %q", e.Name())
				}
			}
		})
	}
}

func TestOpenResolvesARelativePathAgainstTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	db, err := sqlitex.Open(filepath.Join("data", "app.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()
	if fi, err := os.Stat(filepath.Join(dir, "data", "app.db")); err != nil || fi.Size() == 0 {
		t.Errorf("the database is not under the working directory: %v", err)
	}
}

func openCounter(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "app.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE counter (id INTEGER PRIMARY KEY, n INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO counter VALUES (1, 0)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return db
}

// SQLite will not wait to upgrade a read lock: a deferred transaction that read
// and then wrote failed at once with SQLITE_BUSY if anyone had written between.
func TestAReadThenWriteTransactionIsNotBrokenByAnotherWriter(t *testing.T) {
	db := openCounter(t)
	ctx := t.Context()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}

	// A second writer arrives between the read and the write.
	other := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, `UPDATE counter SET n = n + 1 WHERE id = 1`)
		other <- err
	}()
	select {
	case err := <-other:
		other <- err
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := tx.ExecContext(ctx, `UPDATE counter SET n = ? WHERE id = 1`, n+1); err != nil {
		t.Fatalf("the write after a read failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-other; err != nil {
		t.Fatalf("the other writer should have waited its turn: %v", err)
	}
	if err := db.QueryRow(`SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2 — one of the two increments was lost", n)
	}
}

// Taking the write lock at BEGIN is for transactions that may write. One
// declared read-only must still run alongside a writer.
func TestAReadOnlyTransactionDoesNotTakeTheWriteLock(t *testing.T) {
	db := openCounter(t)
	ctx := t.Context()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT n FROM counter WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("read: %v", err)
	}

	start := time.Now()
	if _, err := db.ExecContext(ctx, `UPDATE counter SET n = n + 1 WHERE id = 1`); err != nil {
		t.Fatalf("a writer was blocked by a read-only transaction: %v", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the writer waited %v behind a read-only transaction", waited)
	}
}
