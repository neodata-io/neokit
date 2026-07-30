package fiberx_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/neodata-io/neokit/fiberx"
)

var errCustom = errors.New("a caller's own sentinel")

func mapper(err error) (int, string, string, bool) {
	if errors.Is(err, errCustom) {
		return http.StatusTeapot, "brewing", "teapot", true
	}
	return 0, "", "", false
}

func body(t *testing.T, app *fiber.App, path string) (int, map[string]any) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return resp.StatusCode, out
}

func TestWriteErrorUsesTheInjectedMapper(t *testing.T) {
	e := fiberx.NewErrors(mapper)
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error { return e.WriteError(c, errCustom) })

	status, out := body(t, app, "/x")
	if status != http.StatusTeapot {
		t.Errorf("status = %d, want 418", status)
	}
	if out["error"] != "brewing" {
		t.Errorf("error = %v, want \"brewing\"", out["error"])
	}
}

func TestWriteErrorFallsBackWhenTheMapperDeclines(t *testing.T) {
	e := fiberx.NewErrors(mapper)
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error { return e.WriteError(c, errors.New("unmapped")) })

	status, out := body(t, app, "/x")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
	// The real cause must never reach the client — it is logged instead.
	if got, _ := out["error"].(string); got == "unmapped" {
		t.Error("the internal cause leaked into the response body")
	}
}

func TestNilMapperIsUsable(t *testing.T) {
	e := fiberx.NewErrors(nil) // a project with no domain sentinels yet
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error { return e.WriteError(c, errors.New("boom")) })

	if status, _ := body(t, app, "/x"); status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
}
