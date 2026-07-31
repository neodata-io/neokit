package fiberauth

import (
	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/oidcauth"
)

// localIdentityKey stashes the resolved *oidcauth.Identity on the request. It is
// unexported so nothing outside this package can forge one by writing to a
// well-known key — [WithIdentity] is the deliberate, documented exception.
const localIdentityKey = "neokit_oidc_identity"

// ResolveIdentity recognises the signed-in user from the session cookie and
// stashes the identity for [Gate.RequireOwner], [Gate.Whoami] and handlers.
//
// It never blocks a request — enforcement is a guard's job, and a global
// middleware that rejected would have to know which routes are public. Mount it
// once, before your routes.
//
// When no login is configured it returns immediately: no cookie is read, the
// session store is not touched, and nothing is allocated. That is the "no cost
// when unused" property, and it is asserted by a benchmark.
func (g *Gate) ResolveIdentity() fiber.Handler {
	return func(c fiber.Ctx) error {
		provider := g.Provider()
		if provider == nil {
			return c.Next()
		}
		// The cookie's name depends on the deployment scheme, and the provider is
		// what knows the scheme — the same source the callback used when it wrote
		// the cookie, so the two can never disagree.
		if id := g.resolveSession(c, provider.CookieSecure()); id != nil {
			c.Locals(localIdentityKey, id)
		}
		return c.Next()
	}
}

// resolveSession turns the session cookie into an identity, rolling the session
// forward when it has not been touched recently. It returns nil for an absent,
// unknown, or expired cookie.
func (g *Gate) resolveSession(c fiber.Ctx, secure bool) *oidcauth.Identity {
	token := c.Cookies(g.sessionCookieName(secure))
	if token == "" || g.sessions == nil {
		return nil // the overwhelmingly common path: no cookie, no database read
	}
	ctx := c.Context()
	sess, ok, err := g.sessions.SessionByToken(ctx, oidcauth.HashToken(token))
	if err != nil {
		g.logger().WarnContext(ctx, "session lookup failed", logx.Err(err))
		return nil
	}
	if !ok {
		return nil
	}

	now := g.now().UTC()
	// Expiry and the absolute cap are re-checked here, not merely trusted to the
	// store: SessionStore is an interface, and a security property must not depend
	// on which implementation is wired in. A store that returns a row verbatim
	// would otherwise silently authenticate an expired session, and nothing in
	// this middleware's own tests would notice.
	if !g.policy.Live(sess, now) {
		return nil
	}
	if g.policy.NeedsTouch(sess, now) {
		// ExpiryFor clamps the sliding renewal to the absolute cap, which is what
		// lets a sweeper collect the row on schedule instead of it sitting
		// unreachable — rejected at read time but never expired — forever.
		if err := g.sessions.TouchSession(ctx, sess.ID, now, g.policy.ExpiryFor(sess.CreatedAt, now)); err != nil {
			// Freshness is not worth failing a request the user is entitled to.
			g.logger().WarnContext(ctx, "session touch failed", logx.Err(err))
		}
	}

	id := sess.Identity()
	return &id
}

// RequireOwner guards a route for the signed-in owner.
//
// It passes through when no login is configured, which is what makes the whole
// gate additive: an application that ships open stays open until credentials are
// set, and switching them off restores that exactly. Otherwise it answers 401
// (no session) or 403 (signed in, not an owner).
//
// Apply it to your own route groups. See the package doc for why this package
// does not hold a list of admin paths.
func (g *Gate) RequireOwner() fiber.Handler {
	return func(c fiber.Ctx) error {
		if !g.Enabled() {
			return c.Next() // login not configured → open
		}
		id, ok := IdentityFrom(c)
		switch {
		case !ok:
			return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
		case !id.Owner:
			return fiber.NewError(fiber.StatusForbidden, "owner access required")
		}
		return c.Next()
	}
}

// RequireAuth guards a route for any signed-in user, without the owner check.
// Like [Gate.RequireOwner] it passes through when no login is configured.
func (g *Gate) RequireAuth() fiber.Handler {
	return func(c fiber.Ctx) error {
		if !g.Enabled() {
			return c.Next()
		}
		if _, ok := IdentityFrom(c); !ok {
			return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
		}
		return c.Next()
	}
}

// requireConfigured 404s a route unless a login is configured, so an
// unconfigured deployment exposes no auth surface at all.
//
// [Gate.RequireOwner] alone is not enough for the session-admin routes. It
// deliberately passes through when no login is configured — that is the open
// model — but session rows outlive the gate being switched off. Without this, a
// deployment that once had a login and no longer does would turn every past
// session's name, email and user-agent into unauthenticated, readable PII, and
// let any caller revoke logins.
func (g *Gate) requireConfigured() fiber.Handler {
	return func(c fiber.Ctx) error {
		if !g.Enabled() {
			return fiber.NewError(fiber.StatusNotFound, "login is not configured")
		}
		return c.Next()
	}
}

// IdentityFrom returns the identity resolved for this request, or (nil, false)
// when the request is unauthenticated or no login is configured.
func IdentityFrom(c fiber.Ctx) (*oidcauth.Identity, bool) {
	if v, ok := c.Locals(localIdentityKey).(*oidcauth.Identity); ok && v != nil {
		return v, true
	}
	return nil, false
}

// WithIdentity attaches an identity to a request, the counterpart to
// [IdentityFrom].
//
// [Gate.ResolveIdentity] is the production caller. This is exported for tests:
// the identity lives under a key private to this package, so a handler in
// another package has no way to stand in for the middleware — and the
// alternative, wiring a real session store and provider into every such test,
// exercises this package's plumbing over and over instead of the handler under
// test.
func WithIdentity(c fiber.Ctx, id *oidcauth.Identity) {
	if id == nil {
		return
	}
	c.Locals(localIdentityKey, id)
}
