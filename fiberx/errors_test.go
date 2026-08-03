package fiberx_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/neodata-io/neokit/errs"
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

// With no mapper of its own, a service still gets the standard sentinels right.
// This is the whole point of the errs package: a new service writes no mapping
// code and still answers 404, 400 and 409 correctly.
func TestStandardSentinelsRenderWithoutAMapper(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{errs.ErrNotFound, http.StatusNotFound, "not_found"},
		{errs.ErrInvalidInput, http.StatusBadRequest, "invalid_input"},
		{errs.ErrConflict, http.StatusConflict, "conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			e := fiberx.NewErrors(nil)
			app := fiber.New()
			// Wrapped, because that is how a domain layer returns them in practice.
			app.Get("/x", func(c fiber.Ctx) error {
				return e.WriteError(c, fmt.Errorf("loading horse 7: %w", tc.err))
			})

			status, out := body(t, app, "/x")
			if status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			if out["code"] != tc.code {
				t.Errorf("code = %v, want %q", out["code"], tc.code)
			}
		})
	}
}

// A caller's own mapper still wins outright. Without this ordering a consumer
// could not keep a better public message for a sentinel it shares — NeoGate does
// exactly that for ErrConflict.
func TestAppMapperBeatsTheStandardSet(t *testing.T) {
	e := fiberx.NewErrors(func(err error) (int, string, string, bool) {
		if errors.Is(err, errs.ErrConflict) {
			return http.StatusConflict, "the record was updated concurrently, please retry", "conflict", true
		}
		return 0, "", "", false
	})
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error { return e.WriteError(c, errs.ErrConflict) })

	status, out := body(t, app, "/x")
	if status != http.StatusConflict {
		t.Errorf("status = %d, want 409", status)
	}
	if out["error"] != "the record was updated concurrently, please retry" {
		t.Errorf("error = %v, want the caller's own sentence", out["error"])
	}
}

// A *fiber.Error still outranks everything, including the standard set: it
// carries a deliberate status and a public message that a helper deep in the
// call stack chose on purpose.
func TestFiberErrorStillOutranksTheStandardSet(t *testing.T) {
	e := fiberx.NewErrors(nil)
	app := fiber.New()
	app.Get("/x", func(c fiber.Ctx) error {
		return e.WriteError(c, fmt.Errorf("%w: %w", fiber.NewError(http.StatusGone, "dat paard is verkocht"), errs.ErrNotFound))
	})

	status, out := body(t, app, "/x")
	if status != http.StatusGone {
		t.Errorf("status = %d, want 410 — the standard set must not flatten a deliberate fiber.Error", status)
	}
	if out["error"] != "dat paard is verkocht" {
		t.Errorf("error = %v, want the fiber.Error's message", out["error"])
	}
}
