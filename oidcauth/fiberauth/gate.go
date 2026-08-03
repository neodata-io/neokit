// Package fiberauth is the browser-facing half of [oidcauth]: the Fiber v3
// routes, cookies and middleware that turn a verified identity into a session
// and back again.
//
// It is deliberately separate from oidcauth so the protocol half carries no web
// framework, and it is deliberately small: [Gate] mounts five routes, resolves an
// identity per request, and guards the ones you point at it. It stores nothing
// itself — you supply an [oidcauth.SessionStore] over whatever database you
// already have.
//
// # Optional by construction
//
// [Options.Provider] is a function returning a possibly-nil provider, so "no
// login configured" is a first-class state rather than a second flag that can
// disagree with the credentials. When it returns nil:
//
//   - [Gate.ResolveIdentity] returns immediately, reading no cookie and touching
//     no database;
//   - [Gate.RequireOwner] passes every request through, so an application that
//     ships open stays open;
//   - the handshake and session-admin routes answer 404, so an unconfigured
//     deployment exposes no auth surface at all.
//
// Logout is the one exception: it keeps working, because a browser holding a
// session cookie from before the login was switched off must still have a way to
// clear it.
//
// # Usage
//
//	gate := fiberauth.New(a, fiberauth.Options{
//	    Provider:     func() *oidcauth.Provider { return authn }, // nil ⇒ open
//	    Sessions:     store,
//	    CookiePrefix: "myapp",
//	})
//
//	admin := a.HTTP.Group("/api/v1/admin", gate.RequireOwner())
//
// [New] mounts the identity middleware, then the routes, then declares the gate
// in the boot report — in that order, which is the one a caller cannot arrange by
// hand.
//
// Apply [Gate.RequireOwner] to your own route groups rather than expecting this
// package to hold a list of admin paths: the route definitions already carry
// that knowledge, and a central prefix-matched list both drifts and tends to
// catch public routes that merely share a prefix.
package fiberauth

import (
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/declare"
	"github.com/neodata-io/neokit/oidcauth"
)

// Defaults for the two mount points.
const (
	// DefaultAPIBase is where the API-shaped auth routes (whoami, sessions) hang.
	DefaultAPIBase = "/api/v1"

	// DefaultHandshakeBase is where the three browser-facing handshake routes
	// live, and it is deliberately **unversioned**: they are redirect targets a
	// browser is sent to, not endpoints an API consumer calls. The callback URI is
	// also registered *at the identity provider*, so moving it breaks every
	// sign-in until someone edits the provider to match.
	DefaultHandshakeBase = "/api/auth"
)

// DefaultRateLimit caps handshake attempts per minute per peer.
//
// The callback route makes this server POST the provider's token endpoint, so
// without a limit an unauthenticated caller can use it as an amplifier aimed at
// the identity provider.
const DefaultRateLimit = 30

// handshakeTTL bounds how long a login may sit half-finished.
const handshakeTTL = 10 * time.Minute

// Options configures a [Gate]. Only Provider and Sessions are required.
type Options struct {
	// Provider returns the relying party, or nil when no login is configured.
	//
	// It is a function so that "configured" can be answered at request time and
	// so a caller can box a typed nil into a genuinely nil interface. A nil
	// Options.Provider itself is treated as "never configured".
	Provider func() *oidcauth.Provider

	// Sessions persists signed-in browsers. Required when Provider can return
	// non-nil.
	Sessions oidcauth.SessionStore

	// CookiePrefix namespaces the two cookies: "{prefix}_session" and
	// "{prefix}_auth_state". Defaults to "auth".
	//
	// Choose it once and keep it: changing it signs everyone out, because the
	// browser still holds a cookie under the old name that nothing reads.
	CookiePrefix string

	// Policy is the session lifetime rule. The zero value means
	// [oidcauth.DefaultPolicy].
	Policy oidcauth.Policy

	// APIBase and HandshakeBase override the two mount points. See
	// [DefaultAPIBase] and [DefaultHandshakeBase].
	APIBase       string
	HandshakeBase string

	// RateLimit caps handshake requests per minute per peer. Zero means
	// [DefaultRateLimit]; negative disables it.
	RateLimit int

	// OnLoginFailure renders a failed sign-in. Nil redirects to
	// [Options.LoginFailurePath] when one is set, and otherwise answers a JSON
	// 401.
	//
	// The callback is reached by a *browser navigation*, not by fetch, so its
	// failures have to land on a page — JSON renders raw at someone who is by
	// definition locked out. Supply this (or LoginFailurePath) to give them a page
	// that explains the [Reason] and offers a retry.
	OnLoginFailure func(c fiber.Ctx, reason Reason, cause error) error

	// LoginFailurePath is a convenience for the common case: the browser is sent
	// to "{path}?reason={reason}" and the cause is logged, never travelling.
	// Ignored when OnLoginFailure is set.
	LoginFailurePath string

	// ReasonParam names the query parameter carrying the [Reason] on a
	// LoginFailurePath redirect. Defaults to "reason" — override it to localise
	// the URL.
	ReasonParam string

	// Log receives the gate's diagnostics. Nil means slog.Default().
	Log *slog.Logger
}

// Gate is the mounted login gate. Construct it with [New].
type Gate struct {
	provider func() *oidcauth.Provider
	sessions oidcauth.SessionStore
	policy   oidcauth.Policy
	log      *slog.Logger

	apiBase       string
	handshakeBase string
	rateLimit     int

	sessionCookie string // "{prefix}_session"
	stateCookie   string // "{prefix}_auth_state"

	onFailure func(c fiber.Ctx, reason Reason, cause error) error

	// now is the clock, injectable for tests. Never nil after New.
	now func() time.Time
}

// New builds the gate, wires it into a, and declares it in the boot report. One
// call — there is no separate Register step to forget or to run out of order.
//
// The order it wires in is the point: [Gate.ResolveIdentity] is mounted before
// the handshake routes, because whoami and the session endpoints read the
// identity that middleware resolves. A caller cannot get that order right by
// hand — it needs the gate to obtain the middleware, by which time the routes
// would already be mounted.
//
// It cannot fail: a gate with no Provider is a working gate that is switched off.
//
// The expired-session sweep is part of the same declaration, so a store that can
// prune is pruned without a second call to remember — including when the gate is
// off, since the rows an earlier configuration created outlive it.
func New(a *app.App, o Options) *Gate {
	prefix := strings.TrimSpace(o.CookiePrefix)
	if prefix == "" {
		prefix = "auth"
	}
	g := &Gate{
		provider:      o.Provider,
		sessions:      o.Sessions,
		policy:        o.Policy,
		log:           o.Log,
		apiBase:       orDefault(o.APIBase, DefaultAPIBase),
		handshakeBase: orDefault(o.HandshakeBase, DefaultHandshakeBase),
		rateLimit:     o.RateLimit,
		sessionCookie: prefix + "_session",
		stateCookie:   prefix + "_auth_state",
		onFailure:     o.OnLoginFailure,
		now:           time.Now,
	}
	if g.rateLimit == 0 {
		g.rateLimit = DefaultRateLimit
	}
	if g.onFailure == nil {
		g.onFailure = defaultFailureHandler(o.LoginFailurePath, orDefault(o.ReasonParam, "reason"))
	}

	// Middleware first, then routes: whoami and the session endpoints call
	// IdentityFrom, which reads what ResolveIdentity puts in Locals, and Fiber
	// only runs middleware registered ahead of a route.
	a.HTTP.Use(g.ResolveIdentity())
	g.register(a.HTTP)

	if !g.Enabled() {
		declare.Add(a, "login", declare.Disabled("not configured"))
		// A store outlives the login that filled it, and nothing else prunes it —
		// so the sweep still runs, under its own name because there is no login
		// line left to hang it off. Stating it is the point: a sweep with no
		// login configured is otherwise unexplainable.
		if job, ok := oidcauth.SweepJob(g.sessions, g.logger()); ok {
			declare.Add(a, "session sweep",
				declare.Detail("pruning expired sessions; no login configured"),
				declare.Run(job.Run))
		}
		return g
	}
	opts := []declare.Option{declare.Detail(g.Provider().Issuer())}
	// Attached to the login line rather than declared beside it: one feature,
	// one name.
	if job, ok := oidcauth.SweepJob(g.sessions, g.logger()); ok {
		opts = append(opts, declare.Run(job.Run))
	}
	declare.Add(a, "login", opts...)
	return g
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v == "" {
		return def
	}
	return v
}

func (g *Gate) logger() *slog.Logger {
	if g.log != nil {
		return g.log
	}
	return slog.Default()
}

// Provider returns the relying party, or nil when no login is configured.
func (g *Gate) Provider() *oidcauth.Provider {
	if g.provider == nil {
		return nil
	}
	return g.provider()
}

// Enabled reports whether a login is configured.
func (g *Gate) Enabled() bool { return g.Provider() != nil }

// LoginPath is where a browser starts the handshake. [Gate.Whoami] returns it to
// the client, so it is a method rather than a literal: a second copy would part
// company the first time the base path moves.
func (g *Gate) LoginPath() string { return g.handshakeBase + "/login" }

// CallbackPath is the OIDC redirect URI's path. It must agree with the
// provider's [oidcauth.Config.CallbackPath]; [Gate.Register] checks this.
func (g *Gate) CallbackPath() string { return g.handshakeBase + "/callback" }

// LogoutPath ends the local session.
func (g *Gate) LogoutPath() string { return g.handshakeBase + "/logout" }

// WhoamiPath reports the signed-in identity.
func (g *Gate) WhoamiPath() string { return g.apiBase + "/auth/whoami" }

// SessionsPath is the owner-only session list.
func (g *Gate) SessionsPath() string { return g.apiBase + "/auth/sessions" }

// HashToken exposes [oidcauth.HashToken] so a caller holding a raw token — an
// administrative tool, a test — hashes it exactly as the gate does.
func (g *Gate) HashToken(token string) string { return oidcauth.HashToken(token) }
