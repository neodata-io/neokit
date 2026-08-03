package session_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver the tests open with

	"github.com/neodata-io/neokit/session"
)

// A file under TempDir, never a bare :memory: — a memory database is private
// per pooled connection, so a write and a read can land on different databases.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The DDL runs on every boot, so it has to be idempotent — that is what lets
// construction create the schema instead of an application migration doing it.
func TestNewSQLiteIsIdempotent(t *testing.T) {
	db := testDB(t)

	if _, err := session.NewSQLite(db); err != nil {
		t.Fatalf("first NewSQLite: %v", err)
	}
	if _, err := session.NewSQLite(db); err != nil {
		t.Fatalf("second NewSQLite: %v", err)
	}

	var name string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'auth_session'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("auth_session was not created: %v", err)
	}
}

// A nil database is a wiring mistake, and it has to fail here rather than at the
// first login months later.
func TestNewSQLiteRejectsANilDatabase(t *testing.T) {
	if _, err := session.NewSQLite(nil); err == nil {
		t.Error("NewSQLite(nil) must fail")
	}
}

// A stamped session, so every column has something to round-trip.
func aSession() session.Session {
	t0 := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	return session.Session{
		ID: "sess-1", Subject: "user-1", Name: "Ada", Email: "ada@example.com",
		Groups: []string{"admins", "staff"}, Owner: true, UserAgent: "Firefox",
		CreatedAt: t0, LastSeenAt: t0, ExpiresAt: t0.Add(24 * time.Hour),
	}
}

func newStore(t *testing.T) *session.SQLite {
	t.Helper()
	s, err := session.NewSQLite(testDB(t))
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	return s
}

func TestCreateAndResolveByToken(t *testing.T) {
	store := newStore(t)
	want := aSession()
	if err := store.CreateSession(context.Background(), want, session.HashToken("raw-token")); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, ok, err := store.SessionByToken(context.Background(), session.HashToken("raw-token"))
	if err != nil || !ok {
		t.Fatalf("SessionByToken = (_, %v, %v), want (_, true, nil)", ok, err)
	}
	if got.ID != want.ID || got.Subject != want.Subject || got.Name != want.Name ||
		got.Email != want.Email || got.Owner != want.Owner || got.UserAgent != want.UserAgent {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Groups) != 2 || got.Groups[0] != "admins" || got.Groups[1] != "staff" {
		t.Errorf("Groups = %v, want [admins staff]", got.Groups)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("times = (%v, %v), want (%v, %v)",
			got.CreatedAt, got.ExpiresAt, want.CreatedAt, want.ExpiresAt)
	}
}

// The property the whole design exists for. If this fails, a stolen database
// file is a stolen login — so it reads every column of every row rather than
// trusting that the insert named the right ones.
func TestTheRawTokenNeverReachesTheDatabase(t *testing.T) {
	db := testDB(t)
	store, err := session.NewSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	const raw = "a-very-distinctive-raw-token"
	if err := store.CreateSession(context.Background(), aSession(), session.HashToken(raw)); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT * FROM auth_session`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			t.Fatal(err)
		}
		for i, c := range cells {
			if v := c.(*sql.NullString); v.Valid && strings.Contains(v.String, raw) {
				t.Fatalf("column %q holds the raw token", cols[i])
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// Absent is not an error: a cookie for a session that was revoked or swept is
// the ordinary case, and reporting it as a failure would make every logged-out
// visitor an error line.
func TestAnUnknownTokenIsNotAnError(t *testing.T) {
	store := newStore(t)

	got, ok, err := store.SessionByToken(context.Background(), session.HashToken("never-issued"))
	if err != nil {
		t.Fatalf("SessionByToken error = %v, want nil", err)
	}
	if ok {
		t.Errorf("SessionByToken ok = true for an unknown token, got %+v", got)
	}
}

// An expired row is returned rather than filtered. Expiry is Policy.Live's job;
// a second enforcement point here is how the two come to disagree.
func TestAnExpiredRowIsReturnedAndRejectedByPolicy(t *testing.T) {
	store := newStore(t)
	dead := aSession()
	dead.ExpiresAt = dead.CreatedAt.Add(-time.Hour)
	if err := store.CreateSession(context.Background(), dead, session.HashToken("t")); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.SessionByToken(context.Background(), session.HashToken("t"))
	if err != nil || !ok {
		t.Fatalf("the store must return the row, got (_, %v, %v)", ok, err)
	}
	if session.DefaultPolicy().Live(got, dead.CreatedAt) {
		t.Error("Policy.Live must reject the expired row the store returned")
	}
}
