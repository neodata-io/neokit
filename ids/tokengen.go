package ids

import (
	"crypto/rand"
	"encoding/base64"
)

// CryptoTokenGenerator produces cryptographically random, URL-safe tokens.
type CryptoTokenGenerator struct{}

func (CryptoTokenGenerator) NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// GeneratePassword returns a 16-byte crypto-random base64url string suitable
// for use as a generated service account password.
func (CryptoTokenGenerator) GeneratePassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
