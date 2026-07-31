package fiberx

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every APIError field is exported and there is no constructor, so
// &APIError{Message: "..."} is the obvious way to write one. It used to render
// as HTTP 200 with an error body — c.Status(0) leaves fasthttp's default
// untouched — so a client saw 200 and treated the failure as success.
func TestRender_ZeroStatusAPIErrorBecomesA500(t *testing.T) {
	t.Parallel()

	e := NewErrors(nil)
	app := fiber.New(fiber.Config{ErrorHandler: e.Render})
	app.Get("/", func(fiber.Ctx) error { return &APIError{Message: "nope"} })

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusInternalServerError, resp.StatusCode,
		"an APIError with no Status must not render as 200")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "nope", got["error"])
	assert.NotEmpty(t, got["code"], "an unset code must be derived from the status, never shipped empty")
}

func TestRender_ExplicitStatusIsPreserved(t *testing.T) {
	t.Parallel()

	e := NewErrors(nil)
	app := fiber.New(fiber.Config{ErrorHandler: e.Render})
	app.Get("/", func(fiber.Ctx) error {
		return &APIError{Status: fiber.StatusNotFound, Message: "gone", Code: "custom_code"}
	})

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, fiber.StatusNotFound, resp.StatusCode)

	var got map[string]any
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "custom_code", got["code"], "an explicit code must not be overwritten")
}

// Normalizing must not mutate the caller's error: the same value may be a
// package-level sentinel, rendered on many requests.
func TestRender_DoesNotMutateTheCallersError(t *testing.T) {
	t.Parallel()

	sentinel := &APIError{Message: "nope"}

	e := NewErrors(nil)
	app := fiber.New(fiber.Config{ErrorHandler: e.Render})
	app.Get("/", func(fiber.Ctx) error { return sentinel })

	resp, err := app.Test(httptest.NewRequest("GET", "/", nil))
	require.NoError(t, err)
	resp.Body.Close()

	assert.Zero(t, sentinel.Status, "Render must not write back into the error it was given")
	assert.Empty(t, sentinel.Code)
}

func TestStatusForError_ZeroStatusIsReportedAs500(t *testing.T) {
	t.Parallel()
	// Otherwise the metrics middleware records the request under status="0".
	assert.Equal(t, fiber.StatusInternalServerError,
		NewErrors(nil).StatusForError(&APIError{Message: "nope"}))
}
