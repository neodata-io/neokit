package oidcauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// Identity is the authenticated principal derived from a verified ID token. A
// zero value (Subject == "") means "not authenticated".
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
	// Owner reports administrative rights: membership in [Config.OwnerGroup], or
	// true for every authenticated identity when no owner group is configured.
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

// SessionStore persists sessions. An application implements it over whatever
// storage it already has — this package ships none, so it never forces a second
// database or a schema you did not choose.
//
// tokenHash is always the output of [HashToken]; the raw token must never be
// written down.
//
// An implementation may filter expired rows on read, but is not required to:
// expiry is re-checked by the middleware, so the security property does not
// depend on which store is wired in.
type SessionStore interface {
	CreateSession(ctx context.Context, s Session, tokenHash string) error
	SessionByToken(ctx context.Context, tokenHash string) (Session, bool, error)
	TouchSession(ctx context.Context, id string, lastSeen, expires time.Time) error
	DeleteSession(ctx context.Context, id string) error
	DeleteSessionByToken(ctx context.Context, tokenHash string) error
	ListSessions(ctx context.Context) ([]Session, error)
}

// ExpiredSweeper is the optional companion to [SessionStore]: bulk collection of
// dead rows, for a store that can do it in one statement.
//
// It is separate because it is housekeeping rather than a security control —
// expiry is enforced on read, so a row that outlives its expiry authenticates
// nobody even if the sweep never runs.
type ExpiredSweeper interface {
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}

// Policy is the session lifetime rules. Use [DefaultPolicy] and adjust.
type Policy struct {
	// TTL is how long a session lives without use. Every visit rolls it forward.
	TTL time.Duration

	// MaxLifetime caps a session's total age no matter how active it is.
	//
	// Without it the sliding renewal has no ceiling, so a browser checking in
	// once a month stays signed in forever. Owner and Groups are the snapshot
	// taken at login and are never re-derived, so this is what eventually forces
	// a fresh handshake — the real bound on how long a revoked identity keeps
	// access, and the cost of raising it.
	MaxLifetime time.Duration

	// TouchInterval is the minimum gap between last-seen writes. Without it every
	// authenticated request would write to the database.
	TouchInterval time.Duration
}

// DefaultPolicy is a 30-day idle expiry under a 30-day absolute cap, with
// activity recorded at most hourly.
//
// The two 30s are intentional: idle expiry can only end a session sooner, so the
// cap is what governs. Thirty days of possibly stale authorization is the trade
// — see [Policy.MaxLifetime].
func DefaultPolicy() Policy {
	return Policy{
		TTL:           30 * 24 * time.Hour,
		MaxLifetime:   30 * 24 * time.Hour,
		TouchInterval: time.Hour,
	}
}

// withDefaults fills any unset field from [DefaultPolicy], so a caller can
// override one value without restating the rest.
func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.TTL <= 0 {
		p.TTL = d.TTL
	}
	if p.MaxLifetime <= 0 {
		p.MaxLifetime = d.MaxLifetime
	}
	if p.TouchInterval <= 0 {
		p.TouchInterval = d.TouchInterval
	}
	return p
}

// ExpiryFor returns when a session created at createdAt should expire if
// refreshed at now: the sliding TTL, clamped to the absolute cap.
//
// Clamping is what lets a sweeper collect the row on schedule. Without it a
// renewal can push expires_at past the hard cap, leaving the row rejected at
// read time but never *expired* — unreachable and uncollectable, forever.
func (p Policy) ExpiryFor(createdAt, now time.Time) time.Time {
	p = p.withDefaults()
	expires := now.Add(p.TTL)
	if createdAt.IsZero() {
		return expires
	}
	if hard := createdAt.Add(p.MaxLifetime); expires.After(hard) {
		return hard
	}
	return expires
}

// Live reports whether a session may still authenticate at now, applying both
// the idle expiry and the absolute cap.
//
// Both are re-checked here rather than trusted to the store: [SessionStore] is
// an interface, and a security property must not depend on which implementation
// is wired in. A store that returns a row verbatim would otherwise silently
// authenticate an expired session.
//
// A zero CreatedAt is treated as "unknown, not expired" so a row written before
// the cap existed is retired by the TTL rather than rejected outright.
func (p Policy) Live(s Session, now time.Time) bool {
	p = p.withDefaults()
	if !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(now) {
		return false
	}
	if !s.CreatedAt.IsZero() && now.Sub(s.CreatedAt) >= p.MaxLifetime {
		return false
	}
	return true
}

// NeedsTouch reports whether the session's last-seen timestamp is stale enough
// to be worth a write.
func (p Policy) NeedsTouch(s Session, now time.Time) bool {
	return now.Sub(s.LastSeenAt) >= p.withDefaults().TouchInterval
}

// HashToken is the one-way transform between the cookie's token and what the
// store holds. It is exported so a login handler, a resolver, and any
// out-of-band session tool can never disagree about it.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Handshake is the three per-login secrets: the CSRF state, the ID-token nonce,
// and the PKCE verifier.
//
// They belong somewhere only the browser that started the login can return them
// from — a short-lived, host-only cookie. Never put them in the OAuth state
// parameter itself: state crosses two other origins and comes back in a URL, so
// anything carried there is visible and editable by whoever is watching.
type Handshake struct {
	State    string
	Nonce    string
	Verifier string
}

// NewHandshake mints a fresh set of login secrets.
//
// A failure is returned, never swallowed. Falling back to anything predictable
// would make the state and nonce guessable, which is precisely the forgery the
// two exist to prevent.
func NewHandshake() (Handshake, error) {
	var h Handshake
	var err error
	if h.State, err = RandomToken(32); err != nil {
		return Handshake{}, err
	}
	if h.Nonce, err = RandomToken(32); err != nil {
		return Handshake{}, err
	}
	// 32 random bytes is 43 base64url characters — a valid PKCE verifier, which
	// RFC 7636 §4.1 requires to be 43–128 characters.
	if h.Verifier, err = RandomToken(32); err != nil {
		return Handshake{}, err
	}
	return h, nil
}

// Valid reports whether all three secrets are present.
//
// A blank nonce is the dangerous one: it would sail through the `idToken.Nonce
// != nonce` comparison as "" != "" against a token carrying no nonce claim,
// silently disabling the replay check. [Provider.Exchange] rejects it too — this
// is the caller-side half of that defence.
func (h Handshake) Valid() bool {
	return h.State != "" && h.Nonce != "" && h.Verifier != ""
}

// RandomToken returns n bytes of URL-safe randomness, for a state, a nonce, a
// PKCE verifier, or a session token.
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: crypto/rand failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
