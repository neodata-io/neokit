package sqlitex

import (
	"context"
	"database/sql"
)

// Querier is the read half of *sql.DB, satisfied also by *sql.Tx and *sql.Conn,
// so a helper here works the same inside a transaction as outside one.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Each runs q and calls fn once per row. It closes the rows and reports the
// iteration error, which is the pair a hand-written loop most often drops:
// without rows.Err a result set cut short by a driver error reads as a complete,
// shorter one.
//
// Use it to build something other than a slice — a map, a sum, a lookup. For a
// slice use [Query]; for a single row [QueryOne].
func Each(ctx context.Context, db Querier, q string, args []any, fn func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Query runs q and collects every row through scan.
//
// scan receives the *sql.Rows positioned on a row and returns the value for it:
//
//	links, err := sqlitex.Query(ctx, db, `SELECT id, name FROM links WHERE user_id = ?`,
//		[]any{userID},
//		func(r *sql.Rows) (Link, error) {
//			var l Link
//			return l, r.Scan(&l.ID, &l.Name)
//		})
//
// A query matching nothing returns a nil slice and a nil error.
func Query[T any](ctx context.Context, db Querier, q string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	var out []T
	err := Each(ctx, db, q, args, func(r *sql.Rows) error {
		v, err := scan(r)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// QueryOne returns the first row scanned through scan, or [sql.ErrNoRows] when
// the query matches nothing — the same sentinel (*sql.Row).Scan reports, so a
// caller already branching on it does not change.
//
// Rows after the first are ignored; say so in the query with LIMIT 1 when that
// is what you mean.
func QueryOne[T any](ctx context.Context, db Querier, q string, args []any, scan func(*sql.Rows) (T, error)) (T, error) {
	var (
		out   T
		found bool
	)
	err := Each(ctx, db, q, args, func(r *sql.Rows) error {
		if found {
			return nil
		}
		v, err := scan(r)
		if err != nil {
			return err
		}
		out, found = v, true
		return nil
	})
	if err != nil {
		var zero T
		return zero, err
	}
	if !found {
		var zero T
		return zero, sql.ErrNoRows
	}
	return out, nil
}
