package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// timeLayout is how instants are stored: RFC3339 in UTC, which sorts
// lexicographically, so the sweep's ranged DELETE works on a TEXT column.
const timeLayout = "2006-01-02T15:04:05Z07:00"

// schema is applied on every construction. IF NOT EXISTS rather than a
// sqlitex.Migrate step because that step's index is its version and the
// application owns that sequence — inserting one here would shift every later
// version in every database already in the field.
//
// Statement by statement, because database/sql does not promise a driver will
// accept several in one Exec.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS auth_session (
		id           TEXT PRIMARY KEY,
		token_hash   TEXT NOT NULL UNIQUE,
		subject      TEXT NOT NULL,
		name         TEXT NOT NULL DEFAULT '',
		email        TEXT NOT NULL DEFAULT '',
		groups_json  TEXT NOT NULL DEFAULT '[]',
		owner        INTEGER NOT NULL DEFAULT 0,
		user_agent   TEXT NOT NULL DEFAULT '',
		created_at   TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		expires_at   TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS auth_session_expires ON auth_session(expires_at)`,
}

// SQLite is a [Store] over any *sql.DB speaking SQLite. It also implements
// [ExpiredSweeper], so a gate schedules the sweep without being asked.
//
// It holds no clock: every instant it writes arrives as a parameter, which
// leaves expiry enforced in the one place already tested for it, [Policy.Live].
type SQLite struct{ db *sql.DB }

// NewSQLite creates the session table and its index, then returns the store.
//
// The column names and the RFC3339 encoding are chosen so an existing
// auth_session table fits without a data migration.
func NewSQLite(db *sql.DB) (*SQLite, error) {
	if db == nil {
		return nil, fmt.Errorf("session: NewSQLite needs a database")
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("session: create schema: %w", err)
		}
	}
	return &SQLite{db: db}, nil
}

// CreateSession stores a new signed-in browser. tokenHash is [HashToken]'s
// output; the token itself never reaches the database.
func (s *SQLite) CreateSession(ctx context.Context, sess Session, tokenHash string) error {
	groups, err := json.Marshal(sess.Groups)
	if err != nil {
		return fmt.Errorf("session: encode groups: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO auth_session
			(id, token_hash, subject, name, email, groups_json, owner, user_agent,
			 created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, tokenHash, sess.Subject, sess.Name, sess.Email, string(groups),
		sess.Owner, sess.UserAgent,
		stamp(sess.CreatedAt), stamp(sess.LastSeenAt), stamp(sess.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("session: create: %w", err)
	}
	return nil
}

// SessionByToken resolves a session from the cookie's hash. A row past its
// expiry is returned like any other — [Policy.Live] is the enforcement point,
// and a second one here is how the two come to disagree.
func (s *SQLite) SessionByToken(ctx context.Context, tokenHash string) (Session, bool, error) {
	row := s.db.QueryRowContext(ctx, selectColumns+` WHERE token_hash = ?`, tokenHash)
	sess, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("session: read: %w", err)
	}
	return sess, true, nil
}

// selectColumns fixes the column order every scan depends on, so the list and
// the single-row read cannot drift apart.
const selectColumns = `
	SELECT id, subject, name, email, groups_json, owner, user_agent,
	       created_at, last_seen_at, expires_at
	FROM auth_session`

// scanner is what QueryRow and Rows have in common, so one scan serves both.
type scanner interface{ Scan(dest ...any) error }

func scan(r scanner) (Session, error) {
	var (
		s                          Session
		groups                     string
		created, lastSeen, expires string
	)
	if err := r.Scan(&s.ID, &s.Subject, &s.Name, &s.Email, &groups, &s.Owner,
		&s.UserAgent, &created, &lastSeen, &expires); err != nil {
		return Session{}, err
	}
	// A row whose groups will not parse yields no groups rather than no session:
	// losing a group is a lesser failure than locking someone out.
	_ = json.Unmarshal([]byte(groups), &s.Groups)
	s.CreatedAt = parse(created)
	s.LastSeenAt = parse(lastSeen)
	s.ExpiresAt = parse(expires)
	return s, nil
}

// stamp and parse are the only place the storage encoding is decided.
func stamp(t time.Time) string { return t.UTC().Format(timeLayout) }

// An unparseable instant yields the zero time, which Policy treats as "unknown,
// not expired" — so a corrupt column retires the session on its TTL rather than
// failing the read.
func parse(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
