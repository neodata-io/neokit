package fiberx

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestETagForBytes_IsStrongQuotedAndStable(t *testing.T) {
	a := ETagForBytes([]byte("picture"))
	assert.Equal(t, a, ETagForBytes([]byte("picture")))
	assert.NotEqual(t, a, ETagForBytes([]byte("Picture")))
	assert.Regexp(t, `^"[0-9a-f]{32}"$`, a, "strong, quoted, 128-bit hex")
}

func TestETagForKey_PartBoundariesMatter(t *testing.T) {
	assert.NotEqual(t, ETagForKey("ab", "c"), ETagForKey("a", "bc"))
	assert.Equal(t, ETagForKey("v1", "plex", "42"), ETagForKey("v1", "plex", "42"))
	assert.NotEqual(t, ETagForKey("v1", "plex", "42"), ETagForKey("v2", "plex", "42"), "a pipeline bump invalidates")
}

func TestETagMatches(t *testing.T) {
	const have = `"abc"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{`"abc"`, true},
		{`W/"abc"`, true},
		{`"zzz", "abc"`, true},
		{`"zzz" , W/"abc"`, true},
		{`"ab"`, false},
		{"*", true},
		{`"zzz"`, false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, ETagMatches(tc.header, have), "header %q", tc.header)
	}
}

func TestSendWithETag(t *testing.T) {
	const etag = `"abc"`
	app := fiber.New()
	app.Get("/pic", func(c fiber.Ctx) error {
		return SendWithETag(c, etag, "private, max-age=60", "image/png", []byte("bytes"))
	})

	t.Run("full response carries validator and policy", func(t *testing.T) {
		resp, err := app.Test(httptest.NewRequest("GET", "/pic", nil))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, fiber.StatusOK, resp.StatusCode)
		assert.Equal(t, etag, resp.Header.Get(fiber.HeaderETag))
		assert.Equal(t, "private, max-age=60", resp.Header.Get(fiber.HeaderCacheControl))
		assert.Equal(t, "image/png", resp.Header.Get(fiber.HeaderContentType))
		body, _ := io.ReadAll(resp.Body)
		assert.Equal(t, "bytes", string(body))
	})

	t.Run("a matching If-None-Match answers 304 without a body", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/pic", nil)
		req.Header.Set(fiber.HeaderIfNoneMatch, `W/"abc"`)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, fiber.StatusNotModified, resp.StatusCode)
		assert.Equal(t, etag, resp.Header.Get(fiber.HeaderETag))
		assert.Equal(t, "private, max-age=60", resp.Header.Get(fiber.HeaderCacheControl))
		body, _ := io.ReadAll(resp.Body)
		assert.Empty(t, body)
	})

	t.Run("a different validator gets the full response", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/pic", nil)
		req.Header.Set(fiber.HeaderIfNoneMatch, `"old"`)
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, fiber.StatusOK, resp.StatusCode)
	})
}
