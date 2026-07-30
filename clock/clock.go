package clock

import (
	"time"
)

// RealClock returns the current wall-clock time.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }
