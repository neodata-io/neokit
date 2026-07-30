// Package ids generates identifiers and secrets: UUIDGenerator for v4 UUIDs
// and CryptoTokenGenerator (see tokengen.go) for crypto-random tokens and
// passwords. Both are structs with methods rather than package functions so
// a caller can inject them behind an interface of its own and substitute a
// fake in tests instead of asserting on real random output.
package ids

import (
	"github.com/google/uuid"
)

// UUIDGenerator produces v4 UUIDs.
type UUIDGenerator struct{}

func (UUIDGenerator) New() string { return uuid.NewString() }
