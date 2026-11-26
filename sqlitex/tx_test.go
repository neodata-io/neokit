package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func txDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "tx.db"), func(db *sql.DB) error {
		_, err := db.Exec(`CREATE TABLE t (v INTEGER)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func rowCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInTxCommitsWhenFnSucceeds(t *testing.T) {
	db := txDB(t)
	err := InTx(t.Context(), db, nil, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO t VALUES (1)`)
		return err
	})
	if err != nil || rowCount(t, db) != 1 {
		t.Fatalf("InTx = %v, rows = %d; want nil and 1", err, rowCount(t, db))
	}
}

func TestInTxRollsBackAndReturnsFnsError(t *testing.T) {
	db := txDB(t)
	boom := errors.New("boom")
	err := InTx(t.Context(), db, nil, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO t VALUES (1)`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("InTx = %v, want fn's own error", err)
	}
	if n := rowCount(t, db); n != 0 {
		t.Errorf("rows = %d after a failed fn, want 0", n)
	}
}

// A panic must not leave the transaction open: it would hold the write lock
// until the connection is reclaimed, and every other writer would time out.
func TestInTxRollsBackOnPanicAndRepanics(t *testing.T) {
	db := txDB(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		_ = InTx(t.Context(), db, nil, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`INSERT INTO t VALUES (1)`)
			panic("boom")
		})
	}()
	if n := rowCount(t, db); n != 0 {
		t.Errorf("rows = %d after a panic, want 0", n)
	}
	// The write lock was released: another write goes through at once.
	if _, err := db.Exec(`INSERT INTO t VALUES (2)`); err != nil {
		t.Errorf("write after the panic: %v", err)
	}
}

// A context cancelled mid-transaction rolls it back. InTx must say so rather
// than report a commit that never happened.
func TestInTxReportsAFailedCommit(t *testing.T) {
	db := txDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	err := InTx(ctx, db, nil, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO t VALUES (1)`)
		cancel()
		// database/sql rolls back on its own goroutine; let it finish first.
		time.Sleep(50 * time.Millisecond)
		return err
	})
	if err == nil {
		t.Error("InTx = nil after its context was cancelled, want the failed commit")
	}
	if n := rowCount(t, db); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}
