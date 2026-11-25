// Package apitoken is the vocabulary for a personal API token — the credential a
// script, a CI job or a native app presents instead of a browser session — and
// one store that persists it.
//
// A token acts as the person who made it: it carries a snapshot of their
// identity, exactly as a session does, and nothing of its own. What makes it
// safe to hand to a script is everything it is not:
//
//   - it is 256 random bits, so only its SHA-256 is stored and a stolen database
//     holds nothing that can be presented;
//   - it is shown once, when made, and cannot be read back;
//   - it starts with a prefix the application chooses, which is how a request
//     knows a bearer value is its business and how a secret scanner recognises
//     one in a repository;
//   - it belongs to the login it was made under (Issuer), so pointing the
//     application at another identity provider retires every token at once.
//
// Like [github.com/neodata-io/neokit/session], it imports only the standard
// library. [github.com/neodata-io/neokit/oidcauth/fiberauth] is what recognises
// a token on a request and mounts the routes that manage them; this package is
// what both of those are written against.
package apitoken

import (
	"context"
	"errors"
	"time"

	"github.com/neodata-io/neokit/session"
)

const (
	// DefaultMaxPerSubject bounds what one person can pile up. A limit, not a
	// quota: nobody needs more, and a script gone wrong stops here.
	DefaultMaxPerSubject = 25

	// NameMaxLen is the longest name a token can carry, in bytes.
	NameMaxLen = 64

	// DefaultTouchInterval is how often a token's last use is written down. A
	// screen shows it to the hour at best, and a script polling every second must
	// not turn every request into a database write.
	DefaultTouchInterval = time.Hour

	entropyBytes = 32
)

// ErrTooMany refuses a token over the per-person limit.
var ErrTooMany = errors.New("apitoken: too many API tokens")

// Token is a personal API token as stored: everything but the secret itself, of
// which only the hash is kept.
//
// Owner is deliberately not among the fields. Whether the person is an owner is
// derived per request from Groups and the owner group in force, so tightening
// that group applies to tokens already issued.
type Token struct {
	ID          string
	Name        string
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
	Groups      []string
	CreatedAt   time.Time
	// LastUsedAt is nil until the token is first presented.
	LastUsedAt *time.Time
	// ExpiresAt is nil for a token that does not expire.
	ExpiresAt *time.Time
}

// Identity is who a request presenting this token acts as. Owner stays false:
// only the caller knows the owner group in force.
func (t Token) Identity() session.Identity {
	return session.Identity{Subject: t.Subject, Name: t.DisplayName, Email: t.Email, Groups: t.Groups}
}

// Live reports whether the token still counts at now. A token expiring exactly
// at now is spent.
//
// This is the enforcement point, and it is here rather than in a store on
// purpose: [Lookup] is an interface, and a security property must not depend on
// which implementation is wired in.
func (t Token) Live(now time.Time) bool {
	return t.ExpiresAt == nil || now.Before(*t.ExpiresAt)
}

// NeedsTouch reports whether a use at now should be written down, given how
// often that is done.
func (t Token) NeedsTouch(now time.Time, every time.Duration) bool {
	return t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) >= every
}

// NewSecret mints a token: the secret, which is shown once and never kept, and
// the hash that is.
func NewSecret(prefix string) (secret, hash string) {
	secret = prefix + session.RandomToken(entropyBytes)
	return secret, Hash(secret)
}

// Hash is the one-way transform between a token and its row — the same one
// sessions use, so there is one answer to "how are these stored". A plain
// SHA-256 is enough because the secret is 256 random bits, not a password:
// there is nothing to guess and so nothing for a slow hash to slow down. Lookups
// go through this hash and never compare the secret itself.
func Hash(secret string) string { return session.HashToken(secret) }

// NewID returns a public id for a token row: opaque, safe in a URL, and
// unrelated to the secret.
func NewID() string { return session.RandomToken(16) }

// Lookup is what recognising a token on a request needs.
type Lookup interface {
	// TokenByHash finds the token a request presented. Found or not, it says
	// nothing about whether that token still counts: the caller checks the issuer
	// and [Token.Live].
	TokenByHash(ctx context.Context, tokenHash string) (Token, bool, error)
	// TouchToken records a token's last use.
	TouchToken(ctx context.Context, id string, at time.Time) error
}

// Store is what managing tokens needs on top of recognising them. [SQLite] is
// the one neokit ships.
type Store interface {
	Lookup

	// CreateToken stores a token under its hash, or answers [ErrTooMany].
	CreateToken(ctx context.Context, t Token, tokenHash string) error
	// ListTokens returns one person's tokens under one login, newest first, and
	// never nil.
	ListTokens(ctx context.Context, issuer, subject string) ([]Token, error)
	// DeleteToken revokes one of this person's tokens. Someone else's is
	// [github.com/neodata-io/neokit/errs.ErrNotFound], the same as one that does
	// not exist, so the answer does not reveal which ids are in use.
	DeleteToken(ctx context.Context, id, issuer, subject string) error

	// ListAllTokens returns everyone's tokens, newest first, and never nil.
	ListAllTokens(ctx context.Context) ([]Token, error)
	// RevokeToken revokes anyone's token.
	RevokeToken(ctx context.Context, id string) error
}
