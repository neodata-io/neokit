package sqlitex_test

import (
	"database/sql"
	"testing"

	"github.com/neodata-io/neokit/sqlitex"
	_ "modernc.org/sqlite"
)

func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func TestMigrateRunsAllStepsAndStampsVersion(t *testing.T) {
	db := openMem(t)
	steps := []sqlitex.Migration{
		{"create_a", func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE a (id INTEGER)"); return err }},
		{"create_b", func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE b (id INTEGER)"); return err }},
	}
	if err := sqlitex.Migrate(db, steps); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
}

func TestMigrateSkipsAlreadyAppliedSteps(t *testing.T) {
	db := openMem(t)
	first := []sqlitex.Migration{
		{"create_a", func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE a (id INTEGER)"); return err }},
	}
	if err := sqlitex.Migrate(db, first); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	// Re-running step 1 would fail with "table a already exists" if it were
	// not skipped — that is the assertion.
	both := append(first, sqlitex.Migration{
		"create_b", func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE b (id INTEGER)"); return err },
	})
	if err := sqlitex.Migrate(db, both); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
}

func TestMigrateRollsBackAFailingStep(t *testing.T) {
	db := openMem(t)
	steps := []sqlitex.Migration{
		{"ok", func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE a (id INTEGER)"); return err }},
		{"boom", func(tx *sql.Tx) error { _, err := tx.Exec("THIS IS NOT SQL"); return err }},
	}
	if err := sqlitex.Migrate(db, steps); err == nil {
		t.Fatal("Migrate: want error, got nil")
	}
	// Step 1 committed, step 2 did not: the version must sit at 1, so a
	// retry resumes at the failing step rather than replaying step 1.
	if got := userVersion(t, db); got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
}
