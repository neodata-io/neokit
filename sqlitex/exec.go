package sqlitex

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/neodata-io/neokit/errs"
)

// The three lines every store writes around an IN clause and a conditional
// write. Small, but each one is a place a hand-written copy has gone wrong: a
// `?, ?, ` with a trailing separator, a []string that cannot be spread into a
// variadic ...any, an UPDATE that matched nothing and answered 204.

// Placeholders returns n comma-separated bind markers — "?, ?, ?" — for an IN
// clause. It returns "" for n <= 0; the caller handles the empty set itself,
// since `IN ()` is a syntax error and whether no ids means no rows or all rows is
// the query's business, not this function's.
func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}

// Args widens a typed slice to the []any a variadic query takes, e.g. the ids
// behind an IN clause built with [Placeholders]:
//
//	q := `SELECT … WHERE user_id IN (` + sqlitex.Placeholders(len(ids)) + `)`
//	rows, err := db.QueryContext(ctx, q, sqlitex.Args(ids)...)
func Args[T any](vals []T) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return out
}

// CheckAffected turns an UPDATE or DELETE that matched no row into
// [errs.ErrNotFound], so a handler answers 404 for an id that does not exist
// instead of a 204 that claims the change was made.
func CheckAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return errs.ErrNotFound
	}
	return nil
}
