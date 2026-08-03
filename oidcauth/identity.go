package oidcauth

import (
	"github.com/neodata-io/neokit/session"
)

// The session vocabulary lives in [session], which imports only the standard
// library — so an application authenticating by password can use it without
// linking this package's OIDC client. These are aliases, not copies: a store
// written against either name satisfies both.
type (
	Identity       = session.Identity
	Session        = session.Session
	SessionStore   = session.Store
	ExpiredSweeper = session.ExpiredSweeper
	Policy         = session.Policy
)

// DefaultPolicy is a 30-day idle expiry under a 30-day absolute cap, with
// activity recorded at most hourly. See [session.DefaultPolicy].
func DefaultPolicy() Policy { return session.DefaultPolicy() }

// HashToken is the one-way transform between the cookie's token and what the
// store holds. See [session.HashToken].
func HashToken(token string) string { return session.HashToken(token) }

// RandomToken returns n bytes of URL-safe randomness. See [session.RandomToken].
func RandomToken(n int) (string, error) { return session.RandomToken(n) }

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
