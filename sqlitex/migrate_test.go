package sqlitex_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

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
	var step1Runs int
	steps := []sqlitex.Migration{
		{"ok", func(tx *sql.Tx) error {
			step1Runs++
			_, err := tx.Exec("CREATE TABLE a (id INTEGER)")
			return err
		}},
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

	// Retry with a corrected step list where step 2 now succeeds. This is
	// the property that actually matters: a retry must resume at the
	// failing step rather than replaying step 1. Prove it two ways —
	// step1Runs (a counter captured in step 1's closure) must still read 1,
	// and step 1's CREATE TABLE would itself error with "table a already
	// exists" if it ran again.
	corrected := []sqlitex.Migration{
		steps[0],
		{"boom", func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE b (id INTEGER)"); return err }},
	}
	if err := sqlitex.Migrate(db, corrected); err != nil {
		t.Fatalf("retry Migrate: %v", err)
	}
	if step1Runs != 1 {
		t.Errorf("step1Runs = %d, want 1 (step 1 must not be replayed on retry)", step1Runs)
	}
	if got := userVersion(t, db); got != 2 {
		t.Errorf("user_version = %d, want 2", got)
	}
	if _, err := db.Exec("INSERT INTO b (id) VALUES (1)"); err != nil {
		t.Errorf("table b should exist after retry: %v", err)
	}
}

// The same race end to end: two handles on one file, as two processes have.
func TestTwoMigratorsStartingTogetherApplyAStepOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	open := func() *sql.DB {
		db, err := sqlitex.Open(path, nil)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	first, second := open(), open()
	if _, err := first.Exec(`CREATE TABLE seed (id INTEGER)`); err != nil {
		t.Fatal(err)
	}

	inStep, release := make(chan struct{}), make(chan struct{})
	seed := func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO seed VALUES (1)`); return err }
	held := []sqlitex.Migration{{Name: "seed", Up: func(tx *sql.Tx) error {
		close(inStep)
		<-release
		return seed(tx)
	}}}

	errs := make(chan error, 2)
	go func() { errs <- sqlitex.Migrate(first, held) }()
	<-inStep
	go func() { errs <- sqlitex.Migrate(second, []sqlitex.Migration{{Name: "seed", Up: seed}}) }()
	// Long enough for the second to have read the old version and be waiting.
	time.Sleep(100 * time.Millisecond)
	close(release)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	var rows int
	if err := first.QueryRow(`SELECT count(*) FROM seed`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("the data step ran %d times, want 1", rows)
	}
}
