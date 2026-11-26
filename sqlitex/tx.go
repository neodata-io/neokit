package sqlitex

import (
	"context"
	"database/sql"
)

// InTx runs fn in a transaction and commits it. It rolls back when fn returns
// an error, which InTx then returns, or panics, which it re-raises.
//
// Every transaction [Open] begins takes the write lock: pass
// &sql.TxOptions{ReadOnly: true} for one that only reads.
func InTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	// Deferred so a panic in fn releases the write lock too.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
