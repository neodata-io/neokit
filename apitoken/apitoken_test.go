package apitoken_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver the tests open with

	"github.com/neodata-io/neokit/apitoken"
	"github.com/neodata-io/neokit/errs"
)

// ── The token itself ────────────────────────────────────────────────────────

func TestASecretCarriesItsPrefixAndIsNotItsHash(t *testing.T) {
	secret, hash := apitoken.NewSecret("ng_")

	if !strings.HasPrefix(secret, "ng_") {
		t.Errorf("secret %q does not start with the prefix", secret)
	}
	// 32 bytes of entropy, base64url without padding, is 43 characters.
	if got := len(strings.TrimPrefix(secret, "ng_")); got != 43 {
		t.Errorf("random part is %d characters, want 43 (256 bits)", got)
	}
	if hash != apitoken.Hash(secret) {
		t.Error("the returned hash is not Hash(secret)")
	}
	if strings.Contains(hash, secret) || hash == secret {
		t.Error("the hash gives the secret away")
	}
	if other, _ := apitoken.NewSecret("ng_"); other == secret {
		t.Error("two secrets came out the same")
	}
}

func TestLiveIsDecidedByTheExpiryAlone(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	cases := []struct {
		name    string
		expires *time.Time
		want    bool
	}{
		{"no expiry", nil, true},
		{"expires later", at(time.Second), true},
		// Spent at the instant it expires, not one tick after.
		{"expires now", at(0), false},
		{"expired", at(-time.Second), false},
	}
	for _, tc := range cases {
		if got := (apitoken.Token{ExpiresAt: tc.expires}).Live(now); got != tc.want {
			t.Errorf("%s: Live = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNeedsTouchIsRateLimited(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	used := func(ago time.Duration) *time.Time { v := now.Add(-ago); return &v }

	if !(apitoken.Token{}).NeedsTouch(now, time.Hour) {
		t.Error("a token never used must be touched on first use")
	}
	if (apitoken.Token{LastUsedAt: used(59 * time.Minute)}).NeedsTouch(now, time.Hour) {
		t.Error("a token used within the interval must not cost a write")
	}
	if !(apitoken.Token{LastUsedAt: used(time.Hour)}).NeedsTouch(now, time.Hour) {
		t.Error("a token last used a full interval ago must be touched")
	}
}

// Owner is never read off the row: only the caller knows the owner group in
// force today.
func TestIdentityNeverClaimsOwnership(t *testing.T) {
	id := apitoken.Token{Subject: "u-1", DisplayName: "Ada", Email: "ada@example.com", Groups: []string{"admins"}}.Identity()
	if id.Owner {
		t.Error("Identity() claimed ownership")
	}
	if id.Subject != "u-1" || id.Name != "Ada" || id.Email != "ada@example.com" || len(id.Groups) != 1 {
		t.Errorf("identity = %+v", id)
	}
}

// ── The store ───────────────────────────────────────────────────────────────

// A file under TempDir, never a bare :memory: — a memory database is private
// per pooled connection, so a write and a read can land on different databases.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newStore(t *testing.T, o apitoken.SQLiteOptions) *apitoken.SQLite {
	t.Helper()
	s, err := apitoken.NewSQLite(testDB(t), o)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	return s
}

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func aToken(id string) apitoken.Token {
	return apitoken.Token{
		ID: id, Name: "ci", Issuer: "https://id.example.com", Subject: "u-1",
		Email: "ada@example.com", DisplayName: "Ada", Groups: []string{"admins", "a,b"},
		CreatedAt: t0,
	}
}

func TestNewSQLiteIsIdempotentAndRejectsNil(t *testing.T) {
	db := testDB(t)
	for range 2 {
		if _, err := apitoken.NewSQLite(db, apitoken.SQLiteOptions{}); err != nil {
			t.Fatalf("NewSQLite: %v", err)
		}
	}
	if _, err := apitoken.NewSQLite(nil, apitoken.SQLiteOptions{}); err == nil {
		t.Error("NewSQLite(nil) must fail")
	}
}

func TestEveryFieldRoundTrips(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	expires := t0.Add(90 * 24 * time.Hour)
	in := aToken("tok-1")
	in.ExpiresAt = &expires
	secret, hash := apitoken.NewSecret("ng_")
	_ = secret

	if err := store.CreateToken(t.Context(), in, hash); err != nil {
		t.Fatal(err)
	}
	out, found, err := store.TokenByHash(t.Context(), hash)
	if err != nil || !found {
		t.Fatalf("TokenByHash: found=%v err=%v", found, err)
	}
	if out.ID != in.ID || out.Name != in.Name || out.Issuer != in.Issuer || out.Subject != in.Subject ||
		out.Email != in.Email || out.DisplayName != in.DisplayName {
		t.Errorf("token = %+v, want %+v", out, in)
	}
	// A group may contain a comma, which is why they are stored as JSON.
	if len(out.Groups) != 2 || out.Groups[1] != "a,b" {
		t.Errorf("groups = %q", out.Groups)
	}
	if !out.CreatedAt.Equal(t0) || out.LastUsedAt != nil {
		t.Errorf("createdAt = %v, lastUsedAt = %v", out.CreatedAt, out.LastUsedAt)
	}
	if out.ExpiresAt == nil || !out.ExpiresAt.Equal(expires) {
		t.Errorf("expiresAt = %v, want %v", out.ExpiresAt, expires)
	}
}

// The property the whole design rests on: a copy of the database holds nothing
// that can be presented. Every column of every row is read, rather than trusting
// that the insert named the right ones.
func TestTheSecretNeverReachesTheDatabase(t *testing.T) {
	db := testDB(t)
	store, err := apitoken.NewSQLite(db, apitoken.SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secret, hash := apitoken.NewSecret("ng_")
	if err := store.CreateToken(t.Context(), aToken("tok-1"), hash); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(`SELECT * FROM api_token`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	seen := 0
	for rows.Next() {
		seen++
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			t.Fatal(err)
		}
		for i, c := range cells {
			if v := c.(*sql.NullString); v.Valid && strings.Contains(v.String, strings.TrimPrefix(secret, "ng_")) {
				t.Fatalf("column %q holds the secret", cols[i])
			}
		}
	}
	if seen != 1 {
		t.Fatalf("read %d rows, want 1: the check above looked at nothing", seen)
	}
}

func TestAnUnknownHashIsNotAnError(t *testing.T) {
	_, found, err := newStore(t, apitoken.SQLiteOptions{}).TokenByHash(t.Context(), apitoken.Hash("ng_nope"))
	if err != nil || found {
		t.Errorf("found=%v err=%v, want not found and no error", found, err)
	}
}

// The store hands an expired row back; Token.Live is what refuses it. Two
// enforcement points is how they come to disagree.
func TestAnExpiredRowIsReturnedAndRejectedByLive(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	past := t0.Add(-time.Hour)
	tok := aToken("tok-1")
	tok.ExpiresAt = &past
	_, hash := apitoken.NewSecret("ng_")
	if err := store.CreateToken(t.Context(), tok, hash); err != nil {
		t.Fatal(err)
	}

	out, found, err := store.TokenByHash(t.Context(), hash)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if out.Live(t0) {
		t.Error("an expired token reads as live")
	}
}

func TestTouchRecordsTheLastUse(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	_, hash := apitoken.NewSecret("ng_")
	if err := store.CreateToken(t.Context(), aToken("tok-1"), hash); err != nil {
		t.Fatal(err)
	}
	used := t0.Add(3 * time.Hour)
	if err := store.TouchToken(t.Context(), "tok-1", used); err != nil {
		t.Fatal(err)
	}
	out, _, _ := store.TokenByHash(t.Context(), hash)
	if out.LastUsedAt == nil || !out.LastUsedAt.Equal(used) {
		t.Errorf("lastUsedAt = %v, want %v", out.LastUsedAt, used)
	}
}

func create(t *testing.T, store *apitoken.SQLite, tok apitoken.Token) {
	t.Helper()
	_, hash := apitoken.NewSecret("ng_")
	if err := store.CreateToken(t.Context(), tok, hash); err != nil {
		t.Fatalf("CreateToken(%s): %v", tok.ID, err)
	}
}

// "Yours" is the login and the person together: the same subject under another
// identity provider is somebody else.
func TestListIsScopedToOnePersonUnderOneLogin(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	mine1, mine2 := aToken("a"), aToken("b")
	mine2.CreatedAt = t0.Add(time.Minute)
	other := aToken("c")
	other.Subject = "u-2"
	elsewhere := aToken("d")
	elsewhere.Issuer = "https://other-idp.example.com"
	for _, tok := range []apitoken.Token{mine1, mine2, other, elsewhere} {
		create(t, store, tok)
	}

	list, err := store.ListTokens(t.Context(), "https://id.example.com", "u-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "b" || list[1].ID != "a" {
		t.Errorf("list = %+v, want b then a (newest first)", list)
	}

	all, err := store.ListAllTokens(t.Context())
	if err != nil || len(all) != 4 {
		t.Errorf("ListAllTokens = %d rows (err %v), want 4", len(all), err)
	}
}

// Two tokens made in the same second still list newest first.
func TestListOrderSurvivesASharedTimestamp(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	create(t, store, aToken("first"))
	create(t, store, aToken("second"))

	list, _ := store.ListTokens(t.Context(), "https://id.example.com", "u-1")
	if len(list) != 2 || list[0].ID != "second" {
		t.Errorf("list = %+v, want second first", list)
	}
}

// The route sends the list as is, and a list is never null.
func TestListOfNothingIsAnEmptySlice(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	list, err := store.ListTokens(t.Context(), "x", "y")
	if err != nil || list == nil || len(list) != 0 {
		t.Errorf("list = %#v (err %v), want an empty non-nil slice", list, err)
	}
	if all, err := store.ListAllTokens(t.Context()); err != nil || all == nil {
		t.Errorf("all = %#v (err %v), want an empty non-nil slice", all, err)
	}
}

// Someone else's token answers exactly like one that does not exist, so the
// answer does not reveal which ids are in use.
func TestDeleteOnlyReachesYourOwn(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	create(t, store, aToken("mine"))
	theirs := aToken("theirs")
	theirs.Subject = "u-2"
	create(t, store, theirs)

	if err := store.DeleteToken(t.Context(), "theirs", "https://id.example.com", "u-1"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("deleting someone else's token: %v, want ErrNotFound", err)
	}
	if err := store.DeleteToken(t.Context(), "ghost", "https://id.example.com", "u-1"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("deleting a missing token: %v, want ErrNotFound", err)
	}
	if err := store.DeleteToken(t.Context(), "mine", "https://id.example.com", "u-1"); err != nil {
		t.Errorf("deleting my own token: %v", err)
	}
	if all, _ := store.ListAllTokens(t.Context()); len(all) != 1 || all[0].ID != "theirs" {
		t.Errorf("left = %+v, want only theirs", all)
	}
}

func TestRevokeReachesAnyone(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	create(t, store, aToken("a"))

	if err := store.RevokeToken(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeToken(t.Context(), "a"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("revoking twice: %v, want ErrNotFound", err)
	}
}

func TestBulkRemoval(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	create(t, store, aToken("a"))
	leaver := aToken("b")
	leaver.Subject = "u-2"
	create(t, store, leaver)

	if err := store.DeleteTokensOfSubject(t.Context(), "u-2"); err != nil {
		t.Fatal(err)
	}
	if all, _ := store.ListAllTokens(t.Context()); len(all) != 1 || all[0].ID != "a" {
		t.Fatalf("after the leaver's removal: %+v", all)
	}
	// Having none is fine.
	if err := store.DeleteTokensOfSubject(t.Context(), "nobody"); err != nil {
		t.Errorf("removing a subject with no tokens: %v", err)
	}
	if err := store.RevokeAllTokens(t.Context()); err != nil {
		t.Fatal(err)
	}
	if all, _ := store.ListAllTokens(t.Context()); len(all) != 0 {
		t.Errorf("after RevokeAllTokens: %+v", all)
	}
}

func TestTheLimitIsPerPersonPerLogin(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{MaxPerSubject: 2})
	create(t, store, aToken("a"))
	create(t, store, aToken("b"))

	_, hash := apitoken.NewSecret("ng_")
	if err := store.CreateToken(t.Context(), aToken("c"), hash); !errors.Is(err, apitoken.ErrTooMany) {
		t.Fatalf("third token: %v, want ErrTooMany", err)
	}
	// Somebody else still has room.
	other := aToken("d")
	other.Subject = "u-2"
	create(t, store, other)
	// And so does the same person once one is gone.
	if err := store.DeleteToken(t.Context(), "a", "https://id.example.com", "u-1"); err != nil {
		t.Fatal(err)
	}
	create(t, store, aToken("c"))
}

// The limit is part of the insert, so requests racing for the last slot cannot
// all win it.
func TestTheLimitHoldsUnderConcurrentCreates(t *testing.T) {
	const limit, attempts = 3, 20
	store := newStore(t, apitoken.SQLiteOptions{MaxPerSubject: limit})

	var wg sync.WaitGroup
	errsCh := make(chan error, attempts)
	for i := range attempts {
		wg.Go(func() {
			_, hash := apitoken.NewSecret("ng_")
			tok := aToken(apitoken.NewID())
			tok.Name = string(rune('a' + i))
			errsCh <- store.CreateToken(t.Context(), tok, hash)
		})
	}
	wg.Wait()
	close(errsCh)

	made := 0
	for err := range errsCh {
		switch {
		case err == nil:
			made++
		case errors.Is(err, apitoken.ErrTooMany):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	list, _ := store.ListTokens(t.Context(), "https://id.example.com", "u-1")
	if made != limit || len(list) != limit {
		t.Errorf("made %d, stored %d, want exactly %d", made, len(list), limit)
	}
}

// A token with no groups stores an empty list, not JSON null: a row that reads
// back as "no groups" must not be one the next read refuses.
func TestATokenWithNoGroupsRoundTrips(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{})
	tok := aToken("a")
	tok.Groups = nil
	_, hash := apitoken.NewSecret("ng_")
	if err := store.CreateToken(t.Context(), tok, hash); err != nil {
		t.Fatal(err)
	}
	out, found, err := store.TokenByHash(t.Context(), hash)
	if err != nil || !found || len(out.Groups) != 0 {
		t.Errorf("found=%v err=%v groups=%q", found, err, out.Groups)
	}
}

// A corrupt expiry must fail the read. Reading it as "never expires" would turn
// a damaged column into a permanent credential.
func TestACorruptExpiryIsNotReadAsNever(t *testing.T) {
	db := testDB(t)
	store, err := apitoken.NewSQLite(db, apitoken.SQLiteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, hash := apitoken.NewSecret("ng_")
	if err := store.CreateToken(t.Context(), aToken("a"), hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE api_token SET expires_at = 'soon'`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.TokenByHash(t.Context(), hash); err == nil || found {
		t.Errorf("found=%v err=%v, want the read to fail", found, err)
	}
}

// An application that already kept tokens in this table, before expiry existed,
// adopts the store without a migration and without a token going dead.
func TestATableFromBeforeExpiryIsAdopted(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`
		CREATE TABLE api_token (
			id TEXT PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
			issuer TEXT NOT NULL, subject TEXT NOT NULL, email TEXT NOT NULL,
			display_name TEXT NOT NULL, "groups" TEXT NOT NULL,
			created_at TEXT NOT NULL, last_used_at TEXT
		)`); err != nil {
		t.Fatal(err)
	}
	_, hash := apitoken.NewSecret("ng_")
	if _, err := db.Exec(`INSERT INTO api_token VALUES ('old', ?, 'ci', 'iss', 'u-1', '', 'Ada', '["admins"]', '2026-11-05T10:00:00Z', NULL)`, hash); err != nil {
		t.Fatal(err)
	}

	store, err := apitoken.NewSQLite(db, apitoken.SQLiteOptions{})
	if err != nil {
		t.Fatalf("NewSQLite on the older table: %v", err)
	}
	// Twice, as on every later boot.
	if _, err := apitoken.NewSQLite(db, apitoken.SQLiteOptions{}); err != nil {
		t.Fatalf("second NewSQLite: %v", err)
	}

	out, found, err := store.TokenByHash(t.Context(), hash)
	if err != nil || !found {
		t.Fatalf("the existing token: found=%v err=%v", found, err)
	}
	if out.ExpiresAt != nil || !out.Live(time.Now()) {
		t.Errorf("an existing token must keep working with no expiry, got %v", out.ExpiresAt)
	}
}

var _ apitoken.Store = (*apitoken.SQLite)(nil)

// An expired token grants nothing, so it must not use up a slot: otherwise a
// person is locked out of making tokens until someone deletes the dead ones.
func TestExpiredTokensDoNotCountTowardTheLimit(t *testing.T) {
	store := newStore(t, apitoken.SQLiteOptions{MaxPerSubject: 1})
	spent := t0.Add(-time.Hour)
	old := aToken("old")
	old.CreatedAt, old.ExpiresAt = t0.Add(-48*time.Hour), &spent
	create(t, store, old)

	create(t, store, aToken("new"))
}
