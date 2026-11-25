package apitoken

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/neodata-io/neokit/errs"
)

// timeLayout is how instants are stored: RFC3339 in UTC, whole seconds.
const timeLayout = time.RFC3339

// schema is applied on every construction. IF NOT EXISTS rather than a
// sqlitex.Migrate step, for the reason session.NewSQLite gives: a step's index
// is its version and the application owns that sequence.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS api_token (
		id           TEXT PRIMARY KEY,
		token_hash   TEXT NOT NULL UNIQUE,
		name         TEXT NOT NULL,
		issuer       TEXT NOT NULL,
		subject      TEXT NOT NULL,
		email        TEXT NOT NULL DEFAULT '',
		display_name TEXT NOT NULL DEFAULT '',
		"groups"     TEXT NOT NULL DEFAULT '[]',
		created_at   TEXT NOT NULL,
		last_used_at TEXT,
		expires_at   TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_api_token_owner ON api_token(issuer, subject)`,
}

const columns = `id, name, issuer, subject, email, display_name, "groups", created_at, last_used_at, expires_at`

// SQLiteOptions configures [NewSQLite]. The zero value is usable.
type SQLiteOptions struct {
	// MaxPerSubject bounds how many unexpired tokens one person may hold under
	// one login.
	// Zero means [DefaultMaxPerSubject].
	MaxPerSubject int
}

// SQLite is a [Store] over any *sql.DB speaking SQLite.
//
// It holds no clock and enforces no expiry: every instant arrives as a
// parameter, and [Token.Live] is the one place a token's lifetime is decided.
type SQLite struct {
	db  *sql.DB
	max int
}

// NewSQLite creates the token table and its index, then returns the store.
//
// A table that predates expiry — the columns above without expires_at — gains
// that column here, empty, so adopting the package on a database that already
// holds tokens needs no migration and no token stops working.
func NewSQLite(db *sql.DB, o SQLiteOptions) (*SQLite, error) {
	if db == nil {
		return nil, errors.New("apitoken: NewSQLite needs a database")
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("apitoken: create schema: %w", err)
		}
	}
	has, err := hasColumn(db, "expires_at")
	if err != nil {
		return nil, fmt.Errorf("apitoken: inspect schema: %w", err)
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE api_token ADD COLUMN expires_at TEXT`); err != nil {
			return nil, fmt.Errorf("apitoken: add expires_at: %w", err)
		}
	}
	if o.MaxPerSubject <= 0 {
		o.MaxPerSubject = DefaultMaxPerSubject
	}
	return &SQLite{db: db, max: o.MaxPerSubject}, nil
}

func hasColumn(db *sql.DB, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('api_token')`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// CreateToken stores a token under its hash. The limit is checked by the insert
// itself, so two requests cannot both see room for one more; a transaction
// around a separate count would only move that race to whichever commits
// second.
func (s *SQLite) CreateToken(ctx context.Context, t Token, tokenHash string) error {
	groups, err := json.Marshal(t.Groups)
	if err != nil {
		return fmt.Errorf("apitoken: encode groups: %w", err)
	}
	if t.Groups == nil {
		groups = []byte("[]")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO api_token
			(id, token_hash, name, issuer, subject, email, display_name, "groups", created_at, expires_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE (SELECT COUNT(*) FROM api_token
		       WHERE issuer = ? AND subject = ?
		         AND (expires_at IS NULL OR expires_at > ?)) < ?`,
		t.ID, tokenHash, t.Name, t.Issuer, t.Subject, t.Email, t.DisplayName, string(groups),
		stamp(t.CreatedAt), stampPtr(t.ExpiresAt),
		// Only live tokens use up a slot: an expired one grants nothing.
		t.Issuer, t.Subject, stamp(t.CreatedAt), s.max,
	)
	if err != nil {
		return fmt.Errorf("apitoken: create: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("apitoken: create: %w", err)
	}
	if n == 0 {
		return ErrTooMany
	}
	return nil
}

// TokenByHash finds the token a request presented. An expired row is returned
// like any other — see [Token.Live].
func (s *SQLite) TokenByHash(ctx context.Context, tokenHash string) (Token, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM api_token WHERE token_hash = ?`, tokenHash)
	t, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, fmt.Errorf("apitoken: read: %w", err)
	}
	return t, true, nil
}

// TouchToken records a token's last use.
func (s *SQLite) TouchToken(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE api_token SET last_used_at = ? WHERE id = ?`, stamp(at), id); err != nil {
		return fmt.Errorf("apitoken: touch: %w", err)
	}
	return nil
}

// ListTokens returns one person's tokens under one login, newest first.
func (s *SQLite) ListTokens(ctx context.Context, issuer, subject string) ([]Token, error) {
	// rowid breaks the tie: created_at has whole seconds, and "newest first"
	// should not depend on what SQLite happens to return for two made in the
	// same one.
	return s.list(ctx, `SELECT `+columns+` FROM api_token
		WHERE issuer = ? AND subject = ? ORDER BY created_at DESC, rowid DESC`, issuer, subject)
}

// ListAllTokens returns everyone's tokens, newest first.
func (s *SQLite) ListAllTokens(ctx context.Context) ([]Token, error) {
	return s.list(ctx, `SELECT `+columns+` FROM api_token ORDER BY created_at DESC, rowid DESC`)
}

func (s *SQLite) list(ctx context.Context, query string, args ...any) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("apitoken: list: %w", err)
	}
	defer rows.Close()

	// Non-nil, so an empty table serialises as [] rather than null.
	out := []Token{}
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("apitoken: list: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apitoken: list: %w", err)
	}
	return out, nil
}

// DeleteToken revokes one of this person's tokens; anything else is
// [errs.ErrNotFound].
func (s *SQLite) DeleteToken(ctx context.Context, id, issuer, subject string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_token WHERE id = ? AND issuer = ? AND subject = ?`, id, issuer, subject)
	if err != nil {
		return fmt.Errorf("apitoken: delete: %w", err)
	}
	return affected(res, "delete")
}

// RevokeToken revokes anyone's token. One that does not exist is
// [errs.ErrNotFound].
func (s *SQLite) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_token WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("apitoken: revoke: %w", err)
	}
	return affected(res, "revoke")
}

// DeleteTokensOfSubject removes every token of one person, under any login —
// for when they leave. Having none is fine.
func (s *SQLite) DeleteTokensOfSubject(ctx context.Context, subject string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM api_token WHERE subject = ?`, subject); err != nil {
		return fmt.Errorf("apitoken: delete of subject: %w", err)
	}
	return nil
}

// RevokeAllTokens clears every token. For a login that changed: tokens made
// under the old one already answer 401, and this removes the rows.
func (s *SQLite) RevokeAllTokens(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM api_token`); err != nil {
		return fmt.Errorf("apitoken: revoke all: %w", err)
	}
	return nil
}

func affected(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("apitoken: %s: %w", op, err)
	}
	if n == 0 {
		return errs.ErrNotFound
	}
	return nil
}

func scan(row interface{ Scan(dest ...any) error }) (Token, error) {
	var (
		t                 Token
		groups, created   string
		lastUsed, expires sql.NullString
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Issuer, &t.Subject, &t.Email, &t.DisplayName,
		&groups, &created, &lastUsed, &expires); err != nil {
		return Token{}, err
	}
	// Unlike a session, a token whose groups will not parse is refused outright.
	// Dropping the groups from a session costs someone their admin rights until
	// they sign in again; here there is no next sign-in to repair it, and a
	// credential that silently changed what it grants is worse than one that
	// stopped working.
	if err := json.Unmarshal([]byte(groups), &t.Groups); err != nil {
		return Token{}, fmt.Errorf("parse groups: %w", err)
	}
	var err error
	if t.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return Token{}, fmt.Errorf("parse created_at: %w", err)
	}
	if t.LastUsedAt, err = parsePtr(lastUsed); err != nil {
		return Token{}, fmt.Errorf("parse last_used_at: %w", err)
	}
	// An expiry that will not parse fails the read, and so the request: reading
	// it as "never expires" would turn a corrupt column into a permanent token.
	if t.ExpiresAt, err = parsePtr(expires); err != nil {
		return Token{}, fmt.Errorf("parse expires_at: %w", err)
	}
	return t, nil
}

func stamp(t time.Time) string { return t.UTC().Format(timeLayout) }

func stampPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(*t)
}

func parsePtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := time.Parse(timeLayout, s.String)
	if err != nil {
		return nil, err
	}
	t = t.UTC()
	return &t, nil
}
