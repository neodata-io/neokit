package httpc

import (
	"sync"
	"time"
)

// DebugEntry records a single outbound HTTP call.
type DebugEntry struct {
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	StatusCode int       `json:"statusCode"`
	DurationMs int64     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
}

// DebugRing is a bounded, thread-safe ring buffer that captures a client's last
// N outbound HTTP requests. It is for debugging visibility — a diagnostics
// endpoint, a support dump — and is intentionally in-memory only (lost on
// restart). Set it as [BaseClient.Debug] to start capturing.
type DebugRing struct {
	mu      sync.Mutex
	entries []DebugEntry
	cap     int
	cursor  int
	full    bool
}

// NewDebugRing creates a ring buffer with the given capacity.
func NewDebugRing(cap int) *DebugRing {
	if cap <= 0 {
		cap = 50
	}
	return &DebugRing{
		entries: make([]DebugEntry, cap),
		cap:     cap,
	}
}

// Push adds an entry to the ring, evicting the oldest if at capacity.
func (r *DebugRing) Push(e DebugEntry) {
	r.mu.Lock()
	r.entries[r.cursor] = e
	r.cursor = (r.cursor + 1) % r.cap
	if r.cursor == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Entries returns all captured entries in chronological order (oldest first).
func (r *DebugRing) Entries() []DebugEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.full {
		out := make([]DebugEntry, r.cursor)
		copy(out, r.entries[:r.cursor])
		return out
	}
	// Ring is full: oldest is at cursor, wrap around.
	out := make([]DebugEntry, r.cap)
	copy(out, r.entries[r.cursor:])
	copy(out[r.cap-r.cursor:], r.entries[:r.cursor])
	return out
}
