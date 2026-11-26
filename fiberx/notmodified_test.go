package fiberx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/fiberx"
)

// A handler whose bytes are expensive answers 304 before producing them.
func TestNotModifiedAnswers304BeforeTheWork(t *testing.T) {
	const etag, cc = `"abc"`, "public, max-age=60"
	worked := false
	app := fiber.New()
	app.Get("/img", func(c fiber.Ctx) error {
		if fiberx.NotModified(c, etag, cc) {
			return nil
		}
		worked = true
		return fiberx.SendWithETag(c, etag, cc, "image/png", []byte("png"))
	})

	req := httptest.NewRequest(http.MethodGet, "/img", nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotModified || worked {
		t.Fatalf("status = %d, worked = %v; want 304 without the work", resp.StatusCode, worked)
	}
	if resp.Header.Get("ETag") != etag || resp.Header.Get("Cache-Control") != cc {
		t.Errorf("headers = %q / %q, want the validator and cache policy on the 304", resp.Header.Get("ETag"), resp.Header.Get("Cache-Control"))
	}
}

func TestNotModifiedLetsAStaleCopyThrough(t *testing.T) {
	app := fiber.New()
	app.Get("/img", func(c fiber.Ctx) error {
		if fiberx.NotModified(c, `"new"`, "no-cache") {
			return nil
		}
		return c.SendString("fresh")
	})

	req := httptest.NewRequest(http.MethodGet, "/img", nil)
	req.Header.Set("If-None-Match", `"old"`)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for a copy that is out of date", resp.StatusCode)
	}
}
