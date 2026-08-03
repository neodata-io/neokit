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

// Touch rolls activity and expiry forward and must leave everything else alone —
// it runs on ordinary requests, where rewriting identity would be a silent
// privilege change.
func TestTouchMovesOnlyTheTimestamps(t *testing.T) {
	store := newStore(t)
	orig := aSession()
	if err := store.CreateSession(context.Background(), orig, session.HashToken("t")); err != nil {
		t.Fatal(err)
	}

	seen := orig.LastSeenAt.Add(2 * time.Hour)
	exp := orig.ExpiresAt.Add(2 * time.Hour)
	if err := store.TouchSession(context.Background(), orig.ID, seen, exp); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}

	got, _, err := store.SessionByToken(context.Background(), session.HashToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSeenAt.Equal(seen) || !got.ExpiresAt.Equal(exp) {
		t.Errorf("timestamps = (%v, %v), want (%v, %v)", got.LastSeenAt, got.ExpiresAt, seen, exp)
	}
	if got.Subject != orig.Subject || got.Owner != orig.Owner || !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Errorf("Touch changed identity or creation: %+v", got)
	}
}

// Revocation by id is the "sign out this device" path; by token is logout.
func TestDeleteByIDAndByToken(t *testing.T) {
	store := newStore(t)
	first, second := aSession(), aSession()
	second.ID = "sess-2"
	if err := store.CreateSession(context.Background(), first, session.HashToken("t1")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), second, session.HashToken("t2")); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteSession(context.Background(), first.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, ok, _ := store.SessionByToken(context.Background(), session.HashToken("t1")); ok {
		t.Error("DeleteSession left the row")
	}
	if _, ok, _ := store.SessionByToken(context.Background(), session.HashToken("t2")); !ok {
		t.Error("DeleteSession removed the wrong row")
	}

	if err := store.DeleteSessionByToken(context.Background(), session.HashToken("t2")); err != nil {
		t.Fatalf("DeleteSessionByToken: %v", err)
	}
	if _, ok, _ := store.SessionByToken(context.Background(), session.HashToken("t2")); ok {
		t.Error("DeleteSessionByToken left the row")
	}
}

// Deleting a session that is already gone is success: logging out twice, or
// revoking a device that expired first, is not a failure to report.
func TestDeletingAnAbsentSessionSucceeds(t *testing.T) {
	store := newStore(t)

	if err := store.DeleteSession(context.Background(), "no-such-id"); err != nil {
		t.Errorf("DeleteSession on an absent row = %v, want nil", err)
	}
	if err := store.DeleteSessionByToken(context.Background(), session.HashToken("nope")); err != nil {
		t.Errorf("DeleteSessionByToken on an absent row = %v, want nil", err)
	}
}

// List includes expired rows. Its caller is a "your devices" screen, which has
// to show a session in order to offer revoking it.
func TestListReturnsEveryRowIncludingExpired(t *testing.T) {
	store := newStore(t)
	live := aSession()
	dead := aSession()
	dead.ID, dead.ExpiresAt = "sess-dead", live.CreatedAt.Add(-time.Hour)
	if err := store.CreateSession(context.Background(), live, session.HashToken("t1")); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), dead, session.HashToken("t2")); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSessions returned %d rows, want 2 including the expired one", len(got))
	}
}

// An empty table is an empty slice, not a nil one: the caller renders it as
// JSON, where nil is `null` and an empty slice is `[]`.
func TestListOfNothingIsAnEmptySlice(t *testing.T) {
	got, err := newStore(t).ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got == nil {
		t.Error("ListSessions returned nil, want an empty slice")
	}
	if len(got) != 0 {
		t.Errorf("ListSessions returned %d rows from an empty table", len(got))
	}
}

// The interface is the contract fiberauth is written against, so satisfying it
// has to be a compile error when it stops being true.
func TestSQLiteIsAStore(t *testing.T) {
	var _ session.Store = (*session.SQLite)(nil)
	var _ session.ExpiredSweeper = (*session.SQLite)(nil)
}

// The sweep is housekeeping: it keeps the table bounded. It must remove exactly
// the dead rows and report how many, because the count is what the job logs.
func TestDeleteExpiredRemovesOnlyDeadRows(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	live := aSession()
	live.ExpiresAt = now.Add(time.Hour)
	dead := aSession()
	dead.ID, dead.ExpiresAt = "sess-dead", now.Add(-time.Hour)
	onTheLine := aSession()
	onTheLine.ID, onTheLine.ExpiresAt = "sess-edge", now

	for i, s := range []session.Session{live, dead, onTheLine} {
		if err := store.CreateSession(context.Background(), s, session.HashToken(string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}

	// A session expiring exactly now is spent, so it goes with the dead one.
	n, err := store.DeleteExpiredSessions(context.Background(), now)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("swept %d rows, want 2", n)
	}

	left, err := store.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != live.ID {
		t.Errorf("remaining = %+v, want only the live session", left)
	}
}
