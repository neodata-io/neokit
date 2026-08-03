// Package session is the vocabulary for a signed-in browser, and one store that
// persists it.
//
// It imports only the standard library so that an application authenticating by
// password can use it without linking an OpenID Connect relying party. [oidcauth]
// aliases every name here, so the two are the same types rather than two sets
// that must be kept in step.
package session

import (
	"context"
	"time"
)

// Identity is the authenticated principal. A zero value (Subject == "") means
// "not authenticated".
type Identity struct {
	// Subject is the stable user id from the provider (the `sub` claim). It is
	// the only claim guaranteed to be present and immutable — key your own user
	// records on this, never on Email, which a provider may let a user change.
	Subject string
	// Name is a display name; it falls back to Subject when the provider sends
	// none, so it is never empty for an authenticated identity.
	Name string
	// Email is the address, when the provider supplies one.
	Email string
	// Groups is the group membership from the configured claim.
	Groups []string
	// Owner reports administrative rights: membership in the configured owner
	// group, or true for every authenticated identity when none is configured.
	Owner bool
}

// Authenticated reports whether the identity names anyone.
func (i Identity) Authenticated() bool { return i.Subject != "" }

// Session is a signed-in browser: the record consulted on every request, so the
// provider is not involved again until it expires.
//
// The cookie's token is deliberately absent. Only its SHA-256 is stored (see
// [HashToken]), so a database leak yields nothing that can be replayed as a
// session.
type Session struct {
	ID         string
	Subject    string
	Name       string
	Email      string
	Groups     []string
	Owner      bool
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// Identity projects the session back to the principal it was minted for.
func (s Session) Identity() Identity {
	return Identity{
		Subject: s.Subject, Name: s.Name, Email: s.Email,
		Groups: s.Groups, Owner: s.Owner,
	}
}

// Store persists sessions. [SQLite] is the implementation this package ships;
// implement it yourself over whatever storage you already have.
//
// tokenHash is always the output of [HashToken]; the raw token must never be
// written down.
//
// An implementation may filter expired rows on read, but is not required to:
// expiry is re-checked by the middleware, so the security property does not
// depend on which store is wired in.
type Store interface {
	CreateSession(ctx context.Context, s Session, tokenHash string) error
	SessionByToken(ctx context.Context, tokenHash string) (Session, bool, error)
	TouchSession(ctx context.Context, id string, lastSeen, expires time.Time) error
	DeleteSession(ctx context.Context, id string) error
	DeleteSessionByToken(ctx context.Context, tokenHash string) error
	ListSessions(ctx context.Context) ([]Session, error)
}

// ExpiredSweeper is the optional companion to [Store]: bulk collection of dead
// rows, for a store that can do it in one statement.
//
// It is separate because it is housekeeping rather than a security control —
// expiry is enforced on read, so a row that outlives its expiry authenticates
// nobody even if the sweep never runs.
type ExpiredSweeper interface {
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}
