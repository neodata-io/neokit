package fiberx

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

// RateLimiter builds a per-minute rate-limit middleware keyed on the real client
// IP. When the app sits behind a reverse proxy, every request otherwise arrives
// from the proxy's own address — Fiber's default key (c.IP()) would then bucket
// every caller behind it into one counter, and two unrelated clients could
// exhaust each other's budget. The proxy forwards the originating client in
// X-Forwarded-For; we key on its first hop and fall back to c.IP() when it's
// absent (direct access), so this never regresses.
//
// XFF is spoofable. That is an acceptable trade when the limiter's job is
// shielding a downstream upstream API from an eager caller (an autocomplete
// hammering a geocoder, say) rather than enforcing a security boundary — see
// [RateLimiterByPeer] for the case where it isn't.
func RateLimiter(maxPerMinute int) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:          maxPerMinute,
		Expiration:   time.Minute,
		KeyGenerator: clientIP,
		LimitReached: limitReached,
	})
}

// limitReached renders a throttled request as the same {"error","code"} envelope
// the rest of the API uses (Fiber's default is a bare "Too Many Requests" string
// the web client can't parse) and attaches Retry-After. The window is a fixed
// minute, so a client that backs off for that long is guaranteed a fresh budget;
// the standard RateLimit-* headers Fiber already emits carry the live counters.
func limitReached(c fiber.Ctx) error {
	c.Set("Retry-After", "60")
	return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
		"error": "rate limit exceeded, please retry shortly",
		"code":  "rate_limited",
	})
}

// RateLimiterByPeer builds a per-minute rate limit keyed on the *transport peer*
// — the address the connection actually came from — ignoring X-Forwarded-For.
//
// Use it for any route that makes its own outbound call to a third party on
// every hit — an OIDC callback exchanging a code at the provider's token
// endpoint is the canonical case. There the limit is what stops an
// unauthenticated caller turning this service into an amplifier, and a budget
// that renews whenever the caller edits a header is a formality, not a limit.
//
// The cost is that every caller behind a shared proxy shares one counter. The
// alternative, Fiber's TrustedProxies, needs the deployment to declare the
// proxy's address — which varies per environment and degrades silently when
// wrong.
func RateLimiterByPeer(maxPerMinute int) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:          maxPerMinute,
		Expiration:   time.Minute,
		KeyGenerator: func(c fiber.Ctx) string { return c.IP() },
		LimitReached: limitReached,
	})
}

// clientIP returns the originating client's address, preferring the first hop of
// X-Forwarded-For (set by the front proxy) over the direct peer. Spoofable by
// construction — see [RateLimiterByPeer] for where that disqualifies it.
func clientIP(c fiber.Ctx) string {
	if xff := c.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return c.IP()
}
