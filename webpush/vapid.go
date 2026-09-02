// Package webpush holds the parts of Web Push (RFC 8030) that are pure key
// handling: generating a VAPID keypair (RFC 8292) and normalising the `sub`
// contact that rides in every VAPID JWT.
//
// It deliberately does **not** send pushes; [github.com/neodata-io/neokit/webpush/delivery]
// does, in a subpackage, so that the dependency a real Web Push implementation
// brings lands only on the callers that actually deliver. Key generation is the
// part every project reimplements and the part that has to survive a restart,
// and it needs nothing.
//
// # No dependencies
//
// A VAPID keypair is an ordinary P-256 keypair in two well-defined encodings:
// the public key is the uncompressed EC point (65 bytes, 0x04‖X‖Y) and the
// private key is the raw 32-byte scalar, both base64url without padding. That is
// four lines of crypto/ecdh, so this package adds nothing to a caller's module
// graph — and the keys it produces are byte-compatible with the established Go
// and JavaScript Web Push libraries, which matters because a keypair is
// persisted on first boot and every existing browser subscription is bound to it.
package webpush

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
)

// Keys is a VAPID keypair in the base64url form that is stored and served.
//
// Public is handed to the browser (it is the `applicationServerKey` passed to
// pushManager.subscribe) and is not secret. Private signs the VAPID JWT and is;
// it must never leave the server.
type Keys struct {
	Public  string
	Private string
}

// Key sizes on the wire, before base64url encoding.
const (
	publicKeyLen  = 65 // 0x04 ‖ X(32) ‖ Y(32), the uncompressed point
	privateKeyLen = 32 // the raw scalar
)

// GenerateKeys creates a new VAPID keypair.
//
// Call it once and persist the result: the public key is baked into every
// browser subscription, so regenerating it silently invalidates every device
// already subscribed — they keep their subscription and simply stop receiving
// anything, with no error anywhere to say why.
func GenerateKeys() (Keys, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return Keys{}, fmt.Errorf("webpush: generate VAPID keys: %w", err)
	}
	return Keys{
		Public:  base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		Private: base64.RawURLEncoding.EncodeToString(priv.Bytes()),
	}, nil
}

// ErrInvalidKey reports a VAPID key that is not a well-formed P-256 key in the
// expected encoding.
var ErrInvalidKey = errors.New("webpush: invalid VAPID key")

// Validate checks that the pair decodes, is the right length, and that the
// public key really is the one derived from the private key.
//
// The last check is the one worth having. A keypair is read back from storage on
// every boot, and a half-written or hand-edited pair produces a server that
// signs every push with a key the browser will not accept — the push service
// answers 401 per subscription, per send, forever. Catching it at startup turns
// that into one clear error.
func (k Keys) Validate() error {
	pub, err := base64.RawURLEncoding.DecodeString(k.Public)
	if err != nil || len(pub) != publicKeyLen {
		return fmt.Errorf("%w: public key must be %d base64url-encoded bytes", ErrInvalidKey, publicKeyLen)
	}
	raw, err := base64.RawURLEncoding.DecodeString(k.Private)
	if err != nil || len(raw) != privateKeyLen {
		return fmt.Errorf("%w: private key must be %d base64url-encoded bytes", ErrInvalidKey, privateKeyLen)
	}
	priv, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		return fmt.Errorf("%w: private key is not a valid P-256 scalar", ErrInvalidKey)
	}
	if base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()) != k.Public {
		return fmt.Errorf("%w: the public key is not derived from the private key", ErrInvalidKey)
	}
	return nil
}

// NormalizeSubject canonicalises the VAPID `sub` claim: the contact a push
// service (Apple, Google, Mozilla) would use to reach the operator about a
// misbehaving sender. RFC 8292 §2.1 requires a "mailto:" or "https:" URI.
//
// It returns the value with any "mailto:" prefix **removed**, because that is
// the form the Go Web Push libraries expect: they prepend "mailto:" to anything
// that is not already an https URL. Handing one a value that already carries the
// prefix produces "mailto:mailto:ops@example.com" — a malformed JWT that Apple
// rejects with a bare `BadJwtToken` and no hint at which claim is wrong. Passing
// the subject through here removes that failure mode regardless of how an admin
// typed it in.
//
// An https URL is returned unchanged. Anything that is neither a usable email
// address nor an https URL is an error, since a push service given an
// unreachable contact may throttle or block the sender outright.
func NormalizeSubject(subject string) (string, error) {
	s := strings.TrimSpace(subject)
	if s == "" {
		return "", errors.New("webpush: VAPID subject is empty")
	}

	if lower := strings.ToLower(s); strings.HasPrefix(lower, "https:") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("webpush: VAPID subject %q is not a valid https URL", subject)
		}
		return s, nil
	}
	// http: is called out separately from "anything else": it is the mistake an
	// admin actually makes, and "use https" is a better answer than "not a valid
	// contact".
	if strings.HasPrefix(strings.ToLower(s), "http:") {
		return "", fmt.Errorf("webpush: VAPID subject %q must use https, not http", subject)
	}

	s = strings.TrimSpace(strings.TrimPrefix(s, "mailto:"))
	if _, err := mail.ParseAddress(s); err != nil {
		return "", fmt.Errorf("webpush: VAPID subject %q is neither an https URL nor an email address", subject)
	}
	return s, nil
}

// Subscription is one browser's Web Push registration: the endpoint the push
// service assigned it, and the two keys its PushSubscription.getKey() returned.
//
// It lives here, next to the keys, rather than in the delivery subpackage,
// because it is vocabulary rather than mechanism — a store persists these, a
// handler receives them from the browser, and neither of those should have to
// import a package that pulls in a Web Push implementation to name the type it
// is already holding.
type Subscription struct {
	// Endpoint is the push service URL to POST to. It identifies the
	// subscription: it is what a store keys on and what a 410 retires.
	Endpoint string
	// Auth is the base64url authentication secret (16 bytes).
	Auth string
	// P256DH is the base64url uncompressed P-256 public key (65 bytes) the
	// payload is encrypted to.
	P256DH string
}
