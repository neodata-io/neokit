package session_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/neodata-io/neokit/oidcauth"
	"github.com/neodata-io/neokit/session"
)

// The store implementing ExpiredSweeper is what makes fiberauth schedule the
// sweep — SweepJob type-asserts for it and returns ok == false otherwise. That
// coupling is invisible at the call site, so it gets a test.
//
// In its own file because it imports oidcauth, which the rest of this package's
// tests deliberately do not.
func TestTheGateWillScheduleTheSweepForThisStore(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := session.NewSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := oidcauth.SweepJob(store, nil); !ok {
		t.Error("SweepJob declined the store, so expired sessions would accumulate forever")
	}
}
