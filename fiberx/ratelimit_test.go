package fiberx

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// Behind a proxy the app has been told to trust, devices sharing the proxy's
// address are limited independently: Fiber derives c.IP() from the header that
// proxy wrote.
func TestRateLimiterSeparatesDevicesBehindATrustedProxy(t *testing.T) {
	app := fiber.New(fiber.Config{
		TrustProxy:         true,
		TrustProxyConfig:   fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0"}}, // app.Test's peer
		ProxyHeader:        fiber.HeaderXForwardedFor,
		EnableIPValidation: true,
	})
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

	get("10.0.0.1")
	get("10.0.0.1")
	if got := get("10.0.0.1"); got != fiber.StatusTooManyRequests {
		t.Errorf("third request from the same device = %d, want 429", got)
	}
	if got := get("10.0.0.2"); got != fiber.StatusOK {
		t.Errorf("first request from a second device = %d, want 200", got)
	}
}

// With no proxy trusted, X-Forwarded-For is whatever the caller typed: rotating
// it must not buy a fresh budget.
func TestRateLimiterIgnoresAForwardedForNobodyVouchedFor(t *testing.T) {
	app := fiber.New()
	app.Get("/x", RateLimiter(2), func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

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
	get("10.0.0.2")
	if got := get("10.0.0.3"); got != fiber.StatusTooManyRequests {
		t.Errorf("third request (rotating X-Forwarded-For) = %d, want 429", got)
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
