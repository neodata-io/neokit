package fiberauth

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/oidcauth"
)

// WhoamiView is the body of the whoami route: who is signed in, and whether a
// login is configured at all.
//
// The identity block is optional because the anonymous case is a normal,
// expected answer. Owner is a *pointer* rather than a plain bool with omitempty:
// an authenticated non-owner must serialize `"owner": false`, and omitempty
// would silently drop exactly that case, leaving the client unable to tell "not
// the owner" from "no identity".
type WhoamiView struct {
	Authenticated bool     `json:"authenticated"`
	SSOEnabled    bool     `json:"ssoEnabled"`
	LoginURL      string   `json:"loginUrl"`
	Subject       string   `json:"subject,omitempty"`
	Name          string   `json:"name,omitempty"`
	Email         string   `json:"email,omitempty"`
	Groups        []string `json:"groups,omitempty"`
	Owner         *bool    `json:"owner,omitempty"`
}

// Whoami reports the signed-in identity so a client can render the current user
// or fall back to the signed-out experience. It relies on
// [Gate.ResolveIdentity] having run first.
//
// LoginURL is always present: the sign-in screen needs somewhere to point
// precisely when nobody is signed in, which is when it is rendered.
func (g *Gate) Whoami() fiber.Handler {
	return func(c fiber.Ctx) error {
		enabled := g.Enabled()
		id, ok := IdentityFrom(c)
		if !ok {
			return c.JSON(WhoamiView{Authenticated: false, SSOEnabled: enabled, LoginURL: g.LoginPath()})
		}
		return c.JSON(WhoamiView{
			Authenticated: true, SSOEnabled: enabled, LoginURL: g.LoginPath(),
			Subject: id.Subject, Name: id.Name, Email: id.Email,
			Groups: id.Groups, Owner: &id.Owner,
		})
	}
}

// SessionView is one signed-in browser as the session list shows it.
//
// The token hash is deliberately absent: nothing about the credential leaves the
// database, not even in a form only an owner can read.
type SessionView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Email      string `json:"email"`
	Owner      bool   `json:"owner"`
	UserAgent  string `json:"userAgent"`
	CreatedAt  string `json:"createdAt"`
	LastSeenAt string `json:"lastSeenAt"`
	Current    bool   `json:"current"`
}

// listSessions returns the signed-in browsers so they can be reviewed and
// revoked. Owner-only, and the whole route 404s when no login is configured —
// see [Gate.requireConfigured] for why the guard alone is not enough.
func (g *Gate) listSessions() fiber.Handler {
	return func(c fiber.Ctx) error {
		list, err := g.sessions.ListSessions(c.Context())
		if err != nil {
			g.logger().ErrorContext(c.Context(), "could not list sessions", "error", err)
			return fiber.NewError(fiber.StatusInternalServerError, "could not list sessions")
		}
		// Mark the caller's own row so a client can label it and refuse to revoke
		// it by accident.
		var currentID string
		if p := g.Provider(); p != nil {
			if token := c.Cookies(g.sessionCookieName(p.CookieSecure())); token != "" {
				if sess, ok, err := g.sessions.SessionByToken(c.Context(), oidcauth.HashToken(token)); err == nil && ok {
					currentID = sess.ID
				}
			}
		}
		out := make([]SessionView, 0, len(list))
		for _, s := range list {
			out = append(out, SessionView{
				ID: s.ID, Name: s.Name, Email: s.Email, Owner: s.Owner,
				UserAgent:  s.UserAgent,
				CreatedAt:  s.CreatedAt.UTC().Format(time.RFC3339),
				LastSeenAt: s.LastSeenAt.UTC().Format(time.RFC3339),
				Current:    s.ID == currentID,
			})
		}
		return c.JSON(out)
	}
}

// revokeSession signs one browser out by deleting its stored session. Revoking
// an already-gone session still succeeds — the caller's intent is satisfied
// either way, and reporting 404 would leak which session ids exist.
func (g *Gate) revokeSession() fiber.Handler {
	return func(c fiber.Ctx) error {
		if err := g.sessions.DeleteSession(c.Context(), c.Params("id")); err != nil {
			g.logger().ErrorContext(c.Context(), "could not revoke the session", "error", err)
			return fiber.NewError(fiber.StatusInternalServerError, "could not revoke the session")
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// The expired-session sweep is not exported here. [New] declares it whether the
// gate is on or off, so a second entry point could only ever start a duplicate
// sweep over the same store. Use [oidcauth.SweepJob] directly if you are not
// mounting a Gate.
