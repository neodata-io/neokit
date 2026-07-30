package ids

import (
	"github.com/google/uuid"
)

// UUIDGenerator produces v4 UUIDs.
type UUIDGenerator struct{}

func (UUIDGenerator) New() string { return uuid.NewString() }
