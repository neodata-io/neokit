package session_test

import (
	"database/sql"
	"testing"

	"github.com/neodata-io/neokit/session"
)

// A row whose expiry or creation time will not parse must not sign anyone in:
// read as the zero time it would never expire and never hit the lifetime cap.
func TestASessionWithAnUnreadableTimestampDoesNotSignIn(t *testing.T) {
	for _, column := range []string{"created_at", "expires_at"} {
		t.Run(column, func(t *testing.T) {
			db := testDB(t)
			store, err := session.NewSQLite(db)
			if err != nil {
				t.Fatal(err)
			}
			hash := session.HashToken("raw-token")
			if err := store.CreateSession(t.Context(), aSession(), hash); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE auth_session SET ` + column + ` = '2021-01-01 00:00:00'`); err != nil {
				t.Fatal(err)
			}

			_, ok, err := store.SessionByToken(t.Context(), hash)
			if ok || err == nil {
				t.Errorf("SessionByToken = (_, %v, %v), want the row refused with an error", ok, err)
			}
		})
	}
}

// The devices list must still show such a row, or it could never be revoked.
func TestTheListStillShowsARowWithAnUnreadableTimestamp(t *testing.T) {
	db := testDB(t)
	store, err := session.NewSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(t.Context(), aSession(), session.HashToken("raw-token")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE auth_session SET expires_at = 'garbage'`); err != nil {
		t.Fatal(err)
	}

	list, err := store.ListSessions(t.Context())
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSessions = (%d rows, %v), want the one row", len(list), err)
	}
}

// The login a session was made under is stored with it, so a gate can retire
// sessions when the identity provider changes.
func TestTheIssuerRoundTrips(t *testing.T) {
	store := newStore(t)
	sess := aSession()
	sess.Issuer = "https://id.example.com"
	hash := session.HashToken("raw-token")
	if err := store.CreateSession(t.Context(), sess, hash); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.SessionByToken(t.Context(), hash)
	if err != nil || !ok {
		t.Fatalf("SessionByToken = (_, %v, %v)", ok, err)
	}
	if got.Issuer != sess.Issuer {
		t.Errorf("Issuer = %q, want %q", got.Issuer, sess.Issuer)
	}
}

// A table from before the issuer column gains it, and its rows keep working.
func TestATableFromBeforeTheIssuerIsAdopted(t *testing.T) {
	db := testDB(t)
	legacy := `CREATE TABLE auth_session (
		id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, subject TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT '', email TEXT NOT NULL DEFAULT '',
		groups_json TEXT NOT NULL DEFAULT '[]', owner INTEGER NOT NULL DEFAULT 0,
		user_agent TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL, expires_at TEXT NOT NULL)`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	hash := session.HashToken("raw-token")
	if _, err := db.Exec(`INSERT INTO auth_session (id, token_hash, subject, created_at, last_seen_at, expires_at)
		VALUES ('old', ?, 'user-1', '2026-08-03T12:00:00Z', '2026-08-03T12:00:00Z', '2026-08-04T12:00:00Z')`, hash); err != nil {
		t.Fatal(err)
	}

	store := adopt(t, db)
	got, ok, err := store.SessionByToken(t.Context(), hash)
	if err != nil || !ok {
		t.Fatalf("SessionByToken = (_, %v, %v), want the old row", ok, err)
	}
	if got.Issuer != "" {
		t.Errorf("Issuer = %q, want empty for a row from before the column", got.Issuer)
	}
	// And a second construction does not try to add the column again.
	adopt(t, db)
}

func adopt(t *testing.T, db *sql.DB) *session.SQLite {
	t.Helper()
	store, err := session.NewSQLite(db)
	if err != nil {
		t.Fatalf("NewSQLite over an existing table: %v", err)
	}
	return store
}
