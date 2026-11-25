package fiberauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/neodata-io/neokit/oidcauth"
)

// requiredGate is newGate with Options.Required set.
func requiredGate(t *testing.T, p *oidcauth.Provider) *Gate {
	t.Helper()
	return New(newTestApp(t), Options{
		Provider:     func() *oidcauth.Provider { return p },
		Sessions:     newMemStore(),
		CookiePrefix: "myapp",
		RateLimit:    -1,
		Required:     true,
	})
}

// A service that must never run open: a missing OIDC setting closes the guarded
// routes instead of opening them.
func TestRequiredGateClosesGuardedRoutesWhenNoLoginIsConfigured(t *testing.T) {
	app := probeApp(requiredGate(t, nil))

	for _, path := range []string{"/admin", "/member"} {
		resp := do(t, app, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s = %d with a required login that is not configured, want 503", path, resp.StatusCode)
		}
	}
}

// Required only changes the guards; a route with no guard was never protected.
func TestRequiredGateLeavesUnguardedRoutesAlone(t *testing.T) {
	app := probeApp(requiredGate(t, nil))

	resp := do(t, app, httptest.NewRequest(http.MethodGet, "/open", nil))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/open = %d, want 200", resp.StatusCode)
	}
}

// Once a login is configured, Required changes nothing: anonymous is still 401.
func TestRequiredGateWithALoginAsksForIt(t *testing.T) {
	app := probeApp(requiredGate(t, testProvider(t, "http://app.test")))

	resp := do(t, app, httptest.NewRequest(http.MethodGet, "/member", nil))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/member = %d, want 401", resp.StatusCode)
	}
}
