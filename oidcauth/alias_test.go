package oidcauth_test

import (
	"testing"

	"github.com/neodata-io/neokit/oidcauth"
	"github.com/neodata-io/neokit/session"
)

// The aliases are the compatibility guarantee, and only assignment in both
// directions proves they are one type rather than two identical ones. A store
// written against either name has to satisfy both.
func TestOIDCNamesAreTheSessionTypes(t *testing.T) {
	var s session.Session = oidcauth.Session{Subject: "u1"}
	var _ oidcauth.Session = s

	var i session.Identity = oidcauth.Identity{Subject: "u1"}
	var _ oidcauth.Identity = i

	var p session.Policy = oidcauth.DefaultPolicy()
	var _ oidcauth.Policy = p

	var store session.Store
	var _ oidcauth.SessionStore = store

	var sweeper session.ExpiredSweeper
	var _ oidcauth.ExpiredSweeper = sweeper

	if oidcauth.HashToken("t") != session.HashToken("t") {
		t.Error("oidcauth.HashToken must be session.HashToken")
	}
}
