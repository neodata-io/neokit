package fiberx

import (
	"net/netip"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/limiter"
)

// RateLimiter builds a per-minute rate limit keyed on the client address,
// c.IP(), counting an IPv6 client by its /64.
//
// Behind a reverse proxy that address is the proxy's until the deployment names
// it in TRUSTED_PROXIES (see [github.com/neodata-io/neokit/config.Base]); every
// caller then shares one budget. X-Forwarded-For is never read directly: a
// header nobody vouched for lets a caller reset its own limit.
func RateLimiter(maxPerMinute int) fiber.Handler {
	return limiter.New(limiter.Config{
		Max:          maxPerMinute,
		Expiration:   time.Minute,
		KeyGenerator: func(c fiber.Ctx) string { return clientKey(c.IP()) },
		LimitReached: limitReached,
	})
}

// clientKey is an IPv4 address itself, and an IPv6 address's /64: one client
// is normally handed a whole /64 and could otherwise rotate through it.
func clientKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if addr = addr.Unmap(); addr.Is4() {
		return addr.String()
	}
	return netip.PrefixFrom(addr.WithZone(""), 64).Masked().String()
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

// RateLimiterByPeer is [RateLimiter].
//
// Deprecated: RateLimiter no longer reads a forwarded header on its own, so the
// two are the same limiter. Use RateLimiter.
func RateLimiterByPeer(maxPerMinute int) fiber.Handler { return RateLimiter(maxPerMinute) }
