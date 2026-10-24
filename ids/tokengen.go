package ids

import (
	"crypto/rand"
	"encoding/base64"
)

// CryptoTokenGenerator produces cryptographically random, URL-safe tokens.
type CryptoTokenGenerator struct{}

// NewToken returns 32 random bytes, base64url-encoded (43 characters).
func (CryptoTokenGenerator) NewToken() string { return random(32) }

// GeneratePassword returns 16 random bytes, base64url-encoded, for a generated
// service-account password.
func (CryptoTokenGenerator) GeneratePassword() string { return random(16) }

// random cannot fail: since Go 1.24 crypto/rand crashes rather than return short.
func random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
