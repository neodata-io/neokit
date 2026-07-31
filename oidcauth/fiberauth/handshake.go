package fiberauth

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/neodata-io/neokit/fiberx"
	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/oidcauth"
)

// Reason is the closed set of causes a sign-in can fail for, and the contract
// between the callback and whatever page explains it.
//
// It is a code rather than a sentence because the copy belongs in the
// application (and in its language), and because a code cannot leak a provider's
// response text into a URL.
type Reason string

const (
	// ReasonExpired: the handshake cookie was missing, malformed, or its state did
	// not match. Usually a stale tab, or a login left sitting past the handshake
	// TTL. Retrying works.
	ReasonExpired Reason = "expired"
	// ReasonDenied: the provider refused this user — typically the account is not
	// in the group allowed to use this client. An administrator must grant access.
	ReasonDenied Reason = "denied"
	// ReasonProvider: any other authorization error from the provider.
	ReasonProvider Reason = "provider"
	// ReasonCode: the token endpoint refused the authorization grant — almost
	// always a code already spent or expired. Nothing to configure; retry.
	ReasonCode Reason = "code"
	// ReasonSecret: the token endpoint refused the client credentials themselves.
	// Kept distinct from ReasonCode: telling someone whose code merely went stale
	// to re-copy their client secret sends them to fix a working setting.
	ReasonSecret Reason = "secret"
	// ReasonToken: the id_token was absent or failed verification. A drifted
	// server clock is the most common benign cause.
	ReasonToken Reason = "token"
	// ReasonUnreachable: the provider could not be reached — discovery or the
	// token exchange never got an answer. A deployment problem, not a credential
	// one.
	ReasonUnreachable Reason = "unreachable"
	// ReasonServer: this server broke — a session write or the random source. Never
	// the administrator's fault, and never something the visitor can fix.
	ReasonServer Reason = "server"
)

// Register mounts the login gate: the three handshake routes, whoami, and the
// owner-only session list.
//
// Login, callback and the session routes 404 when no login is configured.
// Logout is the deliberate exception — it must keep working after an
// administrator switches OIDC off, or a browser holding a session cookie from
// before would have no way to clear it.
//
// The handshake routes are rate-limited per peer rather than by forwarded
// address: they are cheaper to abuse than most, since the callback triggers an
// outbound token exchange per request, and that is exactly the case where a
// budget keyed on a caller-written header is no budget at all.
func (g *Gate) Register(app *fiber.App) {
	// The rate limiter is prepended to each handshake route rather than mounted on
	// a group: Fiber runs the handlers of a route in the order given, so this is
	// the whole chain, and a group would also catch anything else mounted under
	// the same prefix later.
	chain := func(h fiber.Handler) (fiber.Handler, []any) {
		if g.rateLimit < 0 {
			return h, nil
		}
		return fiberx.RateLimiterByPeer(g.rateLimit), []any{h}
	}

	first, rest := chain(g.loginHandler())
	app.Get(g.LoginPath(), first, rest...)
	first, rest = chain(g.callbackHandler())
	app.Get(g.CallbackPath(), first, rest...)
	first, rest = chain(g.logoutHandler())
	app.Post(g.LogoutPath(), first, rest...)

	app.Get(g.WhoamiPath(), g.Whoami())

	configured := g.requireConfigured()
	guard := g.RequireOwner()
	app.Get(g.SessionsPath(), configured, guard, g.listSessions())
	app.Delete(g.SessionsPath()+"/:id", configured, guard, g.revokeSession())
}

// defaultFailureHandler renders a failed sign-in when the caller supplied none.
//
// With a path it redirects there, which is what a browser mid-navigation needs.
// Without one it answers JSON — correct for an API-only deployment, and a nudge
// toward [Options.OnLoginFailure] for anything user-facing.
func defaultFailureHandler(path, param string) func(fiber.Ctx, Reason, error) error {
	if strings.TrimSpace(path) == "" {
		return func(c fiber.Ctx, reason Reason, _ error) error {
			return c.Status(fiber.StatusUnauthorized).
				JSON(fiber.Map{"error": "login failed", "reason": string(reason)})
		}
	}
	return func(c fiber.Ctx, reason Reason, _ error) error {
		return c.Redirect().Status(fiber.StatusFound).
			To(path + "?" + param + "=" + url.QueryEscape(string(reason)))
	}
}

// fail logs the real cause and renders the reason. The cause never travels —
// only the code does.
func (g *Gate) fail(c fiber.Ctx, reason Reason, cause error) error {
	g.logger().WarnContext(c.Context(), "login failed", "reason", string(reason), logx.Err(cause))
	return g.onFailure(c, reason, cause)
}

// reasonFor classifies a failed exchange by sentinel identity, never by parsing
// error text.
func reasonFor(err error) Reason {
	switch {
	case errors.Is(err, oidcauth.ErrClientRejected):
		return ReasonSecret
	case errors.Is(err, oidcauth.ErrCodeRejected):
		return ReasonCode
	case errors.Is(err, oidcauth.ErrTokenInvalid):
		return ReasonToken
	case errors.Is(err, oidcauth.ErrTokenEndpointUnreachable):
		// Listed rather than left to the default: a token request that never landed
		// is the case this mapping most easily gets wrong, and an explicit arm keeps
		// a future sentinel from silently inheriting it.
		return ReasonUnreachable
	default:
		// What is left reaches the provider or fails trying: discovery errors
		// (unresolved host, untrusted certificate) and transport failures.
		return ReasonUnreachable
	}
}

// loginHandler starts the handshake: it mints the state, nonce and PKCE
// verifier, stores them in a short-lived cookie, and redirects to the provider.
//
// A browser navigation, not a fetch — the redirect *is* the response.
func (g *Gate) loginHandler() fiber.Handler {
	return func(c fiber.Ctx) error {
		provider := g.Provider()
		if provider == nil {
			return fiber.NewError(fiber.StatusNotFound, "login is not configured")
		}
		hs, err := oidcauth.NewHandshake()
		if err != nil {
			return g.fail(c, ReasonServer, err)
		}
		authorizeURL, err := provider.AuthorizeURL(c.Context(), hs.State, hs.Nonce, hs.Verifier)
		if err != nil {
			return g.fail(c, ReasonUnreachable, err)
		}

		// Where to land afterwards rides in the handshake cookie rather than in the
		// OAuth state parameter: state crosses two other origins and comes back in a
		// URL, so anything put there is attacker-visible and attacker-editable. The
		// cookie never leaves this host. The path is base64url-encoded so it can
		// never contain the '|' the fields are split on.
		next := base64.RawURLEncoding.EncodeToString([]byte(SafeReturnPath(c.Query("next"))))
		g.setStateCookie(c, strings.Join([]string{hs.State, hs.Nonce, hs.Verifier, next}, "|"), provider.CookieSecure())
		return c.Redirect().Status(fiber.StatusFound).To(authorizeURL)
	}
}

// callbackHandler completes the handshake: it verifies the state against the
// cookie, exchanges the code, and issues the session cookie before redirecting
// to where the login started.
//
// This is the URL registered with the provider. A failure never returns an API
// error by default: the caller is a browser mid-navigation and is by definition
// locked out — see [Options.OnLoginFailure].
func (g *Gate) callbackHandler() fiber.Handler {
	return func(c fiber.Ctx) error {
		provider := g.Provider()
		if provider == nil {
			return fiber.NewError(fiber.StatusNotFound, "login is not configured")
		}
		secure := provider.CookieSecure()
		// Clear the handshake cookie on every outcome: it is single-use, and a
		// lingering one is a replayable state value. Every name it could have gone
		// out under is cleared, because the deployment scheme — and with it the
		// cookie's name — can change while one is still sitting in a browser.
		defer func() {
			for _, name := range g.stateCookieNames() {
				clearCookie(c, name)
			}
		}()

		hs, next, ok := parseStateCookie(c.Cookies(g.stateCookieName(secure)))
		if !ok {
			return g.fail(c, ReasonExpired, errors.New("handshake cookie missing or malformed"))
		}
		// Constant-time comparison is unnecessary: state is single-use, random, and
		// compared against a value the client already holds.
		if c.Query("state") != hs.State {
			return g.fail(c, ReasonExpired, errors.New("state did not match the handshake cookie"))
		}

		// RFC 6749 §4.1.2.1: a denied authorization comes back as
		// ?error=…&error_description=… with the state preserved — not as a missing
		// code. Surfacing the provider's own reason beats the misleading "no code"
		// the check below would otherwise report. Checked only after state
		// validation, so we never reflect an error for a handshake we did not start.
		if e := c.Query("error"); e != "" {
			reason := ReasonProvider
			if e == "access_denied" {
				reason = ReasonDenied
			}
			// The provider's words go to the log, not the URL: they are
			// provider-controlled free text, and the explaining page says the
			// actionable part better than a raw string can.
			g.logger().WarnContext(c.Context(), "login denied by provider",
				"oauth_error", boundedDescription(e),
				"oauth_error_description", boundedDescription(c.Query("error_description")))
			return g.fail(c, reason, nil)
		}
		code := c.Query("code")
		if code == "" {
			return g.fail(c, ReasonProvider, errors.New("the provider returned neither a code nor an error"))
		}

		id, err := provider.Exchange(c.Context(), code, hs.Nonce, hs.Verifier)
		if err != nil {
			return g.fail(c, reasonFor(err), err)
		}

		if err := g.issueSession(c, id, secure); err != nil {
			return g.fail(c, ReasonServer, err)
		}
		return c.Redirect().Status(fiber.StatusFound).To(next)
	}
}

// issueSession mints the session token, persists the session, and sets the cookie.
func (g *Gate) issueSession(c fiber.Ctx, id oidcauth.Identity, secure bool) error {
	token, err := oidcauth.RandomToken(32)
	if err != nil {
		return err
	}
	now := g.now().UTC()
	sess := oidcauth.Session{
		ID: uuid.NewString(), Subject: id.Subject, Name: id.Name, Email: id.Email,
		Groups: id.Groups, Owner: id.Owner,
		UserAgent:  boundedDescription(c.Get("User-Agent")),
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  g.policy.ExpiryFor(now, now),
	}
	if err := g.sessions.CreateSession(c.Context(), sess, oidcauth.HashToken(token)); err != nil {
		return err
	}
	g.logger().InfoContext(c.Context(), "signed in", "subject", id.Subject, "owner", id.Owner)
	g.setSessionCookie(c, token, sess.ExpiresAt, secure)
	return nil
}

// parseStateCookie splits the handshake cookie back into its parts and
// re-validates the return path.
//
// The first three fields must be non-empty, not just state. A blank nonce would
// sail through the exchange's own comparison as "" != "" against a token that
// carries no nonce claim, which is precisely the replay the nonce exists to stop.
// The fourth is allowed to be empty: no destination simply means "/".
//
// The return path is re-validated on the way *out*, not merely on the way in:
// it spent the handshake in a cookie, and a cookie is client state — trusting
// the earlier check would mean trusting the browser to have preserved it.
func parseStateCookie(raw string) (hs oidcauth.Handshake, next string, ok bool) {
	parts := strings.Split(raw, "|")
	if len(parts) != 4 {
		return oidcauth.Handshake{}, "/", false
	}
	hs = oidcauth.Handshake{State: parts[0], Nonce: parts[1], Verifier: parts[2]}
	if !hs.Valid() {
		return oidcauth.Handshake{}, "/", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		decoded = nil
	}
	return hs, SafeReturnPath(string(decoded)), true
}

// boundedDescription makes provider-controlled free text safe to put in a log
// line: whitespace is collapsed — a newline would otherwise let it forge
// additional log entries — and the result is length-bounded so it cannot flood
// the log.
func boundedDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// EndSessionView is the optional second half of signing out: the provider's
// end-session endpoint, which the browser is sent to so the *provider's* session
// ends too, not only this one.
//
// Logout answers 204 whenever there is no such URL — no provider configured, the
// lookup failed, or the provider has no end-session endpoint — so a body is
// present only when there is genuinely a next step to take.
type EndSessionView struct {
	EndSessionURL string `json:"endSessionUrl"`
}

// logoutHandler ends the local session and, when the provider supports it, hands
// back where to go to end the provider's session too.
//
// The order is deliberate and is the whole robustness argument: local sign-out
// happens first and unconditionally, so a provider that is unreachable, has no
// end-session endpoint, or refuses an unregistered post-logout URL can only
// degrade the *second* half. You always end up signed out here.
func (g *Gate) logoutHandler() fiber.Handler {
	return func(c fiber.Ctx) error {
		// Try both names rather than deriving one: the deployment scheme may have
		// changed since this cookie was written (switching the login off flips it),
		// and a logout that cannot find the cookie is a logout that does not happen.
		for _, name := range g.sessionCookieNames() {
			token := c.Cookies(name)
			if token == "" || g.sessions == nil {
				continue
			}
			if err := g.sessions.DeleteSessionByToken(c.Context(), oidcauth.HashToken(token)); err != nil {
				// The cookie is cleared regardless: the browser must end up signed out
				// even if the row outlives it, which a sweeper collects later.
				g.logger().WarnContext(c.Context(), "session delete failed", logx.Err(err))
			}
		}
		for _, name := range g.sessionCookieNames() {
			clearCookie(c, name)
		}

		provider := g.Provider()
		if provider == nil {
			return c.SendStatus(fiber.StatusNoContent)
		}
		endSession, err := provider.EndSessionURL(c.Context())
		if err != nil {
			// Local sign-out already succeeded, so this is not a failed request —
			// only one that cannot offer the second step.
			g.logger().WarnContext(c.Context(), "end-session lookup failed", logx.Err(err))
			return c.SendStatus(fiber.StatusNoContent)
		}
		if endSession == "" {
			return c.SendStatus(fiber.StatusNoContent) // provider has no such endpoint
		}
		return c.JSON(EndSessionView{EndSessionURL: endSession})
	}
}
