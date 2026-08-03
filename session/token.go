package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// HashToken is the one-way transform between the cookie's token and what the
// store holds. It is exported so a login handler, a resolver, and any
// out-of-band session tool can never disagree about it.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// RandomToken returns n bytes of URL-safe randomness, for a session token, a
// state, a nonce, or a PKCE verifier.
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: crypto/rand failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
