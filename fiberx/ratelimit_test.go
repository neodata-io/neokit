package fiberx

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// RateLimiter reads X-Forwarded-For so that, behind a reverse proxy, distinct
// devices sharing the proxy's address are still rate-limited independently
// rather than sharing one counter. That is the entire reason this limiter looks
// at XFF instead of the transport peer.
func TestRateLimiterSeparatesDevicesBehindAProxy(t *testing.T) {
	app := fiber.New()
	app.Get("/x", RateLimiter(2), func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	get := func(xff string) int {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("X-Forwarded-For", xff)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request from %s: %v", xff, err)
		}
		return resp.StatusCode
	}

	// Spend one device's whole budget.
	get("10.0.0.1")
	get("10.0.0.1")
	if got := get("10.0.0.1"); got != fiber.StatusTooManyRequests {
		t.Errorf("third request from the same device = %d, want 429", got)
	}
	// A different device must still be served: one caller exhausting its own
	// budget cannot lock out the next.
	if got := get("10.0.0.2"); got != fiber.StatusOK {
		t.Errorf("first request from a second device = %d, want 200 — per-device fairness is why this limiter reads XFF", got)
	}
}

// RateLimiterByPeer ignores X-Forwarded-For entirely, so a caller cannot buy a
// fresh budget just by rotating a header it fully controls. It exists for a
// route whose limit is a security boundary rather than a courtesy — see the
// type's doc comment for the canonical case (an OIDC callback).
func TestRateLimiterByPeerIgnoresAForgedForwardedFor(t *testing.T) {
	app := fiber.New()
	app.Get("/x", RateLimiterByPeer(2), func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

	get := func(xff string) int {
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("X-Forwarded-For", xff)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request with X-Forwarded-For=%s: %v", xff, err)
		}
		return resp.StatusCode
	}

	get("10.0.0.1")
	get("10.0.0.2") // a different claimed origin, same test peer
	if got := get("10.0.0.3"); got != fiber.StatusTooManyRequests {
		t.Errorf("third request (rotating X-Forwarded-For) = %d, want 429 — the limit is keyed on the peer, not the header", got)
	}
}
