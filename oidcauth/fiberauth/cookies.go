package fiberauth

import (
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

// hostPrefix is the cookie name prefix that makes a cookie host-only, and it is
// the point rather than decoration.
//
// Cookies are not origin-isolated. Without the prefix, anything that can get a
// response attributed to a *parent* domain — a sibling subdomain, or a plain-HTTP
// response an on-path attacker can forge — may set
// `Domain=example.com; myapp_auth_state=…`, which this server then reads as
// indistinguishable from the host-only cookie it wrote itself. The attacker
// plants their own state, nonce and verifier, walks the victim through the
// matching callback, and the victim is silently signed in *as the attacker*:
// login CSRF, whose session-cookie equivalent is session fixation.
//
// `__Host-` closes that in the browser rather than by argument: the user agent
// refuses the cookie unless it is Secure, Path=/, and carries no Domain — so it
// can only have come from this exact host over TLS.
//
// It is applied only on HTTPS deployments, because the prefix mandates Secure,
// which a plain-HTTP LAN deployment cannot set. There the bare name is the only
// option, and the attack it prevents is already available to anyone on that
// network.
const hostPrefix = "__Host-"

// sessionCookieName returns the name the session cookie goes out under.
func (g *Gate) sessionCookieName(secure bool) string {
	if secure {
		return hostPrefix + g.sessionCookie
	}
	return g.sessionCookie
}

// stateCookieName returns the name the in-flight handshake cookie goes out under.
func (g *Gate) stateCookieName(secure bool) string {
	if secure {
		return hostPrefix + g.stateCookie
	}
	return g.stateCookie
}

// sessionCookieNames lists every name a session cookie may have gone out under.
//
// Only ever used to *clear* cookies. Deleting one is always safe, whereas
// **reading** a second name is the attack the prefix exists to remove: accepting
// the unprefixed name as a fallback on an HTTPS deployment hands the attacker
// back exactly the cookie they were going to plant.
//
// Logout needs this because the scheme, and therefore the name, can change under
// a live browser — switching the login off flips `secure` to false while a
// `__Host-` cookie is still on the client, and a logout that could not clear it
// would be a lie.
func (g *Gate) sessionCookieNames() [2]string {
	return [2]string{hostPrefix + g.sessionCookie, g.sessionCookie}
}

func (g *Gate) stateCookieNames() [2]string {
	return [2]string{hostPrefix + g.stateCookie, g.stateCookie}
}

// setSessionCookie writes the session cookie.
func (g *Gate) setSessionCookie(c fiber.Ctx, token string, expires time.Time, secure bool) {
	c.Cookie(&fiber.Cookie{
		Name:     g.sessionCookieName(secure),
		Value:    token,
		Path:     "/",
		HTTPOnly: true, // JS must never read it; that is the XSS containment
		Secure:   secure,
		// Lax, not Strict: the browser arrives back from the provider on a
		// cross-site redirect, and Strict would withhold the cookie on exactly
		// that hop — so the session would be set and then not sent.
		SameSite: fiber.CookieSameSiteLaxMode,
		Expires:  expires,
	})
}

// setStateCookie writes the short-lived handshake cookie.
//
// It is deliberately unsigned. State is a double-submit CSRF check, and an
// attacker who rewrites their own nonce or verifier only breaks their own login.
// What stops them planting one in *someone else's* browser is the `__Host-`
// prefix, not a signature.
func (g *Gate) setStateCookie(c fiber.Ctx, value string, secure bool) {
	c.Cookie(&fiber.Cookie{
		Name:     g.stateCookieName(secure),
		Value:    value,
		Path:     "/",
		HTTPOnly: true,
		Secure:   secure,
		SameSite: fiber.CookieSameSiteLaxMode,
		Expires:  g.now().Add(handshakeTTL),
	})
}

// clearCookie expires the named cookie.
//
// Secure is derived from the name rather than passed in, because a
// `__Host-`-prefixed Set-Cookie *must* carry Secure (plus Path=/ and no Domain)
// or the browser rejects the whole header — so a clear that omitted it was
// silently a no-op on exactly the deployments that use the prefix, which is
// every HTTPS one.
func clearCookie(c fiber.Ctx, name string) {
	c.Cookie(&fiber.Cookie{
		Name: name, Value: "", Path: "/", HTTPOnly: true,
		Secure:   strings.HasPrefix(name, hostPrefix),
		SameSite: fiber.CookieSameSiteLaxMode,
		Expires:  time.Now().Add(-time.Hour), MaxAge: -1,
	})
}

// maxReturnPath bounds the return path a login may carry, so a crafted ?next=
// cannot push the handshake cookie past what a browser will store — which would
// quietly break every login for that visitor.
const maxReturnPath = 512

// SafeReturnPath sanitises a post-login destination, returning "/" for anything
// it does not fully trust.
//
// This is an open-redirect guard, and the interesting cases are the ones that
// look like paths but are not:
//
//   - "//evil.example" is protocol-relative — a browser reads it as another
//     *origin*, so it is an off-site redirect wearing a leading slash.
//   - "/\evil.example" exploits browsers that normalise a backslash to a slash,
//     making it protocol-relative after the fact.
//   - anything carrying a scheme or a host is off-site by construction.
//
// Allowing only a single leading slash, no backslash anywhere, and no control
// characters leaves exactly "a path on this site", which is all a return-to needs
// to be.
//
// Exported because an application with its own post-login redirect — a sign-in
// page that bounces you back where you came from — must apply the identical rule
// rather than a second approximation of it.
func SafeReturnPath(raw string) string {
	if raw == "" || len(raw) > maxReturnPath {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	if strings.ContainsAny(raw, "\\") {
		return "/"
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	// Parse as a final check: a value that survives the above but still carries a
	// scheme or host is not a same-site path.
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return raw
}
