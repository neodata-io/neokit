package session

import "time"

// Policy is the session lifetime rules. Use [DefaultPolicy] and adjust.
type Policy struct {
	// TTL is how long a session lives without use. Every visit rolls it forward.
	TTL time.Duration

	// MaxLifetime caps a session's total age no matter how active it is.
	//
	// Without it the sliding renewal has no ceiling, so a browser checking in
	// once a month stays signed in forever. Owner and Groups are the snapshot
	// taken at login and are never re-derived, so this is what eventually forces
	// a fresh handshake — the real bound on how long a revoked identity keeps
	// access, and the cost of raising it.
	MaxLifetime time.Duration

	// TouchInterval is the minimum gap between last-seen writes. Without it every
	// authenticated request would write to the database.
	TouchInterval time.Duration
}

// DefaultPolicy is a 30-day idle expiry under a 30-day absolute cap, with
// activity recorded at most hourly.
//
// The two 30s are intentional: idle expiry can only end a session sooner, so the
// cap is what governs. Thirty days of possibly stale authorization is the trade
// — see [Policy.MaxLifetime].
func DefaultPolicy() Policy {
	return Policy{
		TTL:           30 * 24 * time.Hour,
		MaxLifetime:   30 * 24 * time.Hour,
		TouchInterval: time.Hour,
	}
}

// withDefaults fills any unset field from [DefaultPolicy], so a caller can
// override one value without restating the rest.
func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.TTL <= 0 {
		p.TTL = d.TTL
	}
	if p.MaxLifetime <= 0 {
		p.MaxLifetime = d.MaxLifetime
	}
	if p.TouchInterval <= 0 {
		p.TouchInterval = d.TouchInterval
	}
	return p
}

// ExpiryFor returns when a session created at createdAt should expire if
// refreshed at now: the sliding TTL, clamped to the absolute cap.
//
// Clamping is what lets a sweeper collect the row on schedule. Without it a
// renewal can push expires_at past the hard cap, leaving the row rejected at
// read time but never *expired* — unreachable and uncollectable, forever.
func (p Policy) ExpiryFor(createdAt, now time.Time) time.Time {
	p = p.withDefaults()
	expires := now.Add(p.TTL)
	if createdAt.IsZero() {
		return expires
	}
	if hard := createdAt.Add(p.MaxLifetime); expires.After(hard) {
		return hard
	}
	return expires
}

// Live reports whether a session may still authenticate at now, applying both
// the idle expiry and the absolute cap.
//
// Both are re-checked here rather than trusted to the store: [Store] is an
// interface, and a security property must not depend on which implementation is
// wired in. A store that returns a row verbatim would otherwise silently
// authenticate an expired session.
//
// A zero CreatedAt is treated as "unknown, not expired" so a row written before
// the cap existed is retired by the TTL rather than rejected outright.
func (p Policy) Live(s Session, now time.Time) bool {
	p = p.withDefaults()
	if !s.ExpiresAt.IsZero() && !s.ExpiresAt.After(now) {
		return false
	}
	if !s.CreatedAt.IsZero() && now.Sub(s.CreatedAt) >= p.MaxLifetime {
		return false
	}
	return true
}

// NeedsTouch reports whether the session's last-seen timestamp is stale enough
// to be worth a write.
func (p Policy) NeedsTouch(s Session, now time.Time) bool {
	return now.Sub(s.LastSeenAt) >= p.withDefaults().TouchInterval
}
