package sqlitex

import (
	"errors"
	"testing"

	"github.com/neodata-io/neokit/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlaceholders(t *testing.T) {
	assert.Equal(t, "", Placeholders(0))
	assert.Equal(t, "", Placeholders(-1))
	assert.Equal(t, "?", Placeholders(1))
	assert.Equal(t, "?, ?, ?", Placeholders(3))
}

func TestArgs(t *testing.T) {
	assert.Equal(t, []any{"a", "b"}, Args([]string{"a", "b"}))
	assert.Equal(t, []any{1, 2, 3}, Args([]int{1, 2, 3}))
	assert.Equal(t, []any{}, Args([]string(nil)), "spreads into a variadic call without a nil check")
}

// fakeResult is a sql.Result with a chosen row count or error.
type fakeResult struct {
	n   int64
	err error
}

func (r fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.n, r.err }

func TestCheckAffected(t *testing.T) {
	assert.NoError(t, CheckAffected(fakeResult{n: 1}))
	assert.ErrorIs(t, CheckAffected(fakeResult{n: 0}), errs.ErrNotFound)

	boom := errors.New("driver cannot count")
	err := CheckAffected(fakeResult{err: boom})
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, errs.ErrNotFound, "a driver failure is not a missing row")
}

// A round trip against a real database, so the three agree with the driver and
// not only with each other.
func TestExecHelpers_AgainstSQLite(t *testing.T) {
	db, err := Open(":memory:", nil)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx := t.Context()

	_, err = db.ExecContext(ctx, `CREATE TABLE t (id TEXT PRIMARY KEY, n INTEGER)`)
	require.NoError(t, err)
	for _, id := range []string{"a", "b", "c"} {
		_, err = db.ExecContext(ctx, `INSERT INTO t (id, n) VALUES (?, 0)`, id)
		require.NoError(t, err)
	}

	ids := []string{"a", "c"}
	res, err := db.ExecContext(ctx, `UPDATE t SET n = 1 WHERE id IN (`+Placeholders(len(ids))+`)`, Args(ids)...)
	require.NoError(t, err)
	require.NoError(t, CheckAffected(res))

	res, err = db.ExecContext(ctx, `DELETE FROM t WHERE id = ?`, "nope")
	require.NoError(t, err)
	assert.ErrorIs(t, CheckAffected(res), errs.ErrNotFound)
}
