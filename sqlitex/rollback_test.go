package sqlitex

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func step(name, ddl string) Migration {
	return Migration{Name: name, Up: func(tx *sql.Tx) error {
		_, err := tx.Exec(ddl)
		return err
	}}
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The rollback corruption vector: deploy a build with more steps, then roll
// back to one with fewer. The migration loop simply does not execute when the
// database is ahead, so the older binary used to start happily against a schema
// it does not understand — and then write to it.
func TestMigrate_RefusesADatabaseNewerThanTheBuild(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	newer := []Migration{
		step("one", "CREATE TABLE a(x)"),
		step("two", "CREATE TABLE b(x)"),
		step("three", "CREATE TABLE c(x)"),
	}
	if err := Migrate(db, newer); err != nil {
		t.Fatalf("forward migration: %v", err)
	}
	if got := userVersion(t, db); got != 3 {
		t.Fatalf("user_version = %d, want 3", got)
	}

	// Now the rolled-back build, which knows only the first step.
	older := []Migration{step("one", "CREATE TABLE a(x)")}
	err := Migrate(db, older)

	if err == nil {
		t.Fatal("a database newer than the build must be refused, not silently accepted")
	}
	if !errorsIs(err, ErrSchemaFromTheFuture) {
		t.Fatalf("error = %v, want ErrSchemaFromTheFuture", err)
	}
	if got := userVersion(t, db); got != 3 {
		t.Fatalf("a refused migration must not touch the file: user_version = %d, want 3", got)
	}
}

func TestMigrate_AcceptsADatabaseAtExactlyTheBuildVersion(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	ms := []Migration{step("one", "CREATE TABLE a(x)")}
	if err := Migrate(db, ms); err != nil {
		t.Fatal(err)
	}
	// Re-running an already-current database is the steady state, not an error.
	if err := Migrate(db, ms); err != nil {
		t.Fatalf("re-running a current database must be a no-op, got %v", err)
	}
}

func TestMigrate_RejectsANilUpWithoutPanicking(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	err := Migrate(db, []Migration{{Name: "broken"}})
	if err == nil {
		t.Fatal("a migration with a nil Up must return an error, not panic")
	}
	// The message must name the step, or an operator cannot find it.
	if !contains(err.Error(), "broken") {
		t.Fatalf("error must name the migration, got %q", err)
	}
}

func TestMigrateContext_HonoursCancellation(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := MigrateContext(ctx, db, []Migration{step("one", "CREATE TABLE a(x)")}); err == nil {
		t.Fatal("a cancelled context must abort the migration")
	}
}

func errorsIs(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
