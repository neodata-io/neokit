package session_test

import (
	"testing"
	"time"

	"github.com/neodata-io/neokit/session"
)

// The sliding TTL must never push expiry past the absolute cap: a row that
// outlives the cap is rejected at read time but never *expires*, so no sweep
// ever collects it.
func TestExpiryForClampsToTheAbsoluteCap(t *testing.T) {
	p := session.Policy{TTL: 30 * 24 * time.Hour, MaxLifetime: 24 * time.Hour}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := created.Add(12 * time.Hour)

	got := p.ExpiryFor(created, now)
	if want := created.Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("ExpiryFor = %v, want the cap %v", got, want)
	}
}

// Live is the enforcement point. It re-checks both bounds rather than trusting
// the store, because Store is an interface and a security property must not
// depend on which implementation is wired in.
func TestLiveRejectsExpiredAndOverAgedSessions(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	p := session.Policy{TTL: time.Hour, MaxLifetime: 24 * time.Hour, TouchInterval: time.Hour}

	expired := session.Session{CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Minute)}
	if p.Live(expired, now) {
		t.Error("a session past ExpiresAt must not be live")
	}

	overAged := session.Session{CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(time.Hour)}
	if p.Live(overAged, now) {
		t.Error("a session past MaxLifetime must not be live, however recently it was used")
	}

	fresh := session.Session{CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	if !p.Live(fresh, now) {
		t.Error("a session inside both bounds must be live")
	}
}

// A zero field means "unset", so one override does not force restating the rest.
func TestZeroFieldsFallBackToDefaults(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	got := session.Policy{TTL: time.Minute}.ExpiryFor(time.Time{}, base)
	if want := base.Add(time.Minute); !got.Equal(want) {
		t.Errorf("ExpiryFor = %v, want the supplied TTL honoured at %v", got, want)
	}
	if session.DefaultPolicy().TouchInterval != time.Hour {
		t.Error("DefaultPolicy must record activity at most hourly")
	}
}

// The hash is what the store holds, so it must be stable and must not be the
// token.
func TestHashTokenIsStableAndNotTheToken(t *testing.T) {
	if session.HashToken("abc") != session.HashToken("abc") {
		t.Error("HashToken must be stable")
	}
	if session.HashToken("abc") == "abc" {
		t.Error("HashToken must not return the token")
	}
	if session.HashToken("abc") == session.HashToken("abd") {
		t.Error("HashToken must distinguish different tokens")
	}
}

// A token that repeats is a token that can be predicted.
func TestRandomTokenIsUnpredictable(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		tok, err := session.RandomToken(32)
		if err != nil {
			t.Fatalf("RandomToken: %v", err)
		}
		if seen[tok] {
			t.Fatal("RandomToken repeated a value")
		}
		seen[tok] = true
	}
}
