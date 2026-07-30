package fiberx

import (
	"net/url"

	"github.com/gofiber/fiber/v3"
)

// PathParam returns a route parameter with its percent-escapes decoded.
//
// Fiber routes on the *original* (escaped) path unless the app sets
// UnescapePath, which we deliberately leave off: turning it on would also decode
// %2F into a path separator and so change how every route matches. The cost is
// that c.Params returns the raw, still-escaped segment — so an id that carries a
// reserved character never reaches the handler intact. A browser sending
// encodeURIComponent("app:netflix") produces "app%3Anetflix", and a plugin
// comparing it against its own "app:netflix" would never match.
//
// Use this for any parameter whose value is opaque and plugin-produced (an action
// id, a favorite id, a cover id). A value with no escapes is returned unchanged,
// and a malformed escape sequence falls back to the raw segment rather than
// erroring — the plugin rejects it as an unknown id, which is the right answer.
func PathParam(c fiber.Ctx, key string) string {
	raw := c.Params(key)
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return decoded
}
