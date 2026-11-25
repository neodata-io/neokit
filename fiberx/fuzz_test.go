package fiberx

import (
	"strconv"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
)

// A malformed If-Match must never be read as a real version: it either means
// "no precondition" or a version that cannot match (-1).
func FuzzIfMatchVersion(f *testing.F) {
	for _, seed := range []string{"", "*", `W/"3"`, `"3"`, `"3", "4"`, `W/"x"`, `"`, ",", `W/`} {
		f.Add(seed)
	}
	app := fiber.New()
	f.Fuzz(func(t *testing.T, header string) {
		c := app.AcquireCtx(&fasthttp.RequestCtx{})
		defer app.ReleaseCtx(c)
		c.Request().Header.Set(fiber.HeaderIfMatch, header)
		// What the handler will actually see: fasthttp normalises header values.
		seen := c.Get(fiber.HeaderIfMatch)

		version, conditional := IfMatchVersion(c)
		if !conditional {
			if version != 0 {
				t.Fatalf("IfMatchVersion(%q) = (%d, false), want version 0 when unconditional", seen, version)
			}
			return
		}
		// A real version must be one CheckIfMatch also accepts for that header.
		if version >= 0 && CheckIfMatch(c, version) != nil {
			t.Fatalf("IfMatchVersion(%q) = %d, but CheckIfMatch refuses %s", seen, version, strconv.Itoa(version))
		}
	})
}

// ETagMatches must not panic on any header, and a tag always matches itself.
func FuzzETagMatches(f *testing.F) {
	f.Add(`"abc"`, `"abc"`)
	f.Add(`W/"abc", "def"`, `"def"`)
	f.Add("*", `"x"`)
	f.Add(",,,", "")
	f.Fuzz(func(t *testing.T, header, etag string) {
		_ = ETagMatches(header, etag)
		if tag := ETagForBytes([]byte(etag)); !ETagMatches(tag, tag) {
			t.Fatalf("ETagMatches(%q, %q) = false", tag, tag)
		}
	})
}
