package sqlitex_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/neodata-io/neokit/sqlitex"
)

type link struct {
	ID   int
	Name string
}

func scanLink(r *sql.Rows) (link, error) {
	var l link
	return l, r.Scan(&l.ID, &l.Name)
}

func linksDB(t *testing.T, n int) *sql.DB {
	t.Helper()

	db, err := sqlitex.Open(&recorder{}, filepath.Join(t.TempDir(), "q.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE links (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, err := db.Exec(`INSERT INTO links (id, name) VALUES (?, ?)`, i, "link"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestQueryCollectsEveryRow(t *testing.T) {
	db := linksDB(t, 3)

	got, err := sqlitex.Query(context.Background(), db,
		`SELECT id, name FROM links ORDER BY id`, nil, scanLink)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 || got[0].ID != 1 || got[2].ID != 3 {
		t.Errorf("rows = %+v, want ids 1..3", got)
	}
}

// A no-match read is an ordinary empty result, not an error — callers append to
// what comes back and must not have to special-case it.
func TestQueryReturnsNilForNoRows(t *testing.T) {
	db := linksDB(t, 0)

	got, err := sqlitex.Query(context.Background(), db,
		`SELECT id, name FROM links`, nil, scanLink)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != nil {
		t.Errorf("rows = %+v, want nil", got)
	}
}

// A scan error must abort rather than yield a short slice that reads as the
// whole result.
func TestQueryPropagatesAScanError(t *testing.T) {
	db := linksDB(t, 3)
	boom := errors.New("scan exploded")

	_, err := sqlitex.Query(context.Background(), db,
		`SELECT id, name FROM links`, nil,
		func(*sql.Rows) (link, error) { return link{}, boom })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the scan error", err)
	}
}

// The sentinel matters: 14 call sites in the first consumer already branch on
// sql.ErrNoRows from (*sql.Row).Scan, and must not have to learn a second one.
func TestQueryOneReportsSQLErrNoRows(t *testing.T) {
	db := linksDB(t, 0)

	_, err := sqlitex.QueryOne(context.Background(), db,
		`SELECT id, name FROM links`, nil, scanLink)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestQueryOneReturnsTheFirstRow(t *testing.T) {
	db := linksDB(t, 3)

	got, err := sqlitex.QueryOne(context.Background(), db,
		`SELECT id, name FROM links ORDER BY id DESC`, nil, scanLink)
	if err != nil {
		t.Fatalf("QueryOne: %v", err)
	}
	if got.ID != 3 {
		t.Errorf("row = %+v, want id 3", got)
	}
}

// Each exists for the results that are not slices — a map keyed by a column is
// the shape that otherwise forces a hand-written loop back into every file.
func TestEachBuildsANonSliceResult(t *testing.T) {
	db := linksDB(t, 3)

	byID := map[int]string{}
	err := sqlitex.Each(context.Background(), db,
		`SELECT id, name FROM links`, nil,
		func(r *sql.Rows) error {
			var id int
			var name string
			if err := r.Scan(&id, &name); err != nil {
				return err
			}
			byID[id] = name
			return nil
		})
	if err != nil {
		t.Fatalf("Each: %v", err)
	}
	if len(byID) != 3 {
		t.Errorf("map = %v, want 3 entries", byID)
	}
}

// The helpers must not cost measurably more than the loop they replace — the
// whole point is that a caller never has to weigh ergonomics against speed.
// Compare against BenchmarkHandRolledScanLoop.
func BenchmarkQuery(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		out, err := sqlitex.Query(ctx, db, `SELECT id, name FROM links`, nil, scanLink)
		if err != nil || len(out) != 100 {
			b.Fatalf("Query: %v (%d rows)", err, len(out))
		}
	}
}

func BenchmarkHandRolledScanLoop(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		rows, err := db.QueryContext(ctx, `SELECT id, name FROM links`)
		if err != nil {
			b.Fatal(err)
		}
		var out []link
		for rows.Next() {
			var l link
			if err := rows.Scan(&l.ID, &l.Name); err != nil {
				rows.Close()
				b.Fatal(err)
			}
			out = append(out, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			b.Fatal(err)
		}
		if len(out) != 100 {
			b.Fatalf("%d rows", len(out))
		}
	}
}

func benchDB(b *testing.B) *sql.DB {
	b.Helper()

	db, err := sqlitex.Open(&recorder{}, filepath.Join(b.TempDir(), "bench.db"), nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE links (id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 100; i++ {
		if _, err := db.Exec(`INSERT INTO links (id, name) VALUES (?, ?)`, i, "link"); err != nil {
			b.Fatal(err)
		}
	}
	return db
}
