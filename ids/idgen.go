// Package ids generates identifiers and secrets. The generators are structs
// rather than functions so a caller can inject them behind its own interface
// and substitute a fake in tests.
package ids

import (
	"github.com/google/uuid"
)

// UUIDGenerator produces v4 UUIDs.
type UUIDGenerator struct{}

// New returns a random (v4) UUID in its canonical string form.
func (UUIDGenerator) New() string { return uuid.NewString() }
