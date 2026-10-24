package fiberx

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// Validators for cacheable binary responses — a proxied poster, a site's
// favicon, a cover — served against If-None-Match. They are strong ETags that
// name exact bytes, which is what makes a 304 safe; the weak version ETags in
// concurrency.go validate an *editable* resource against If-Match and are a
// different contract.

// ETagForBytes derives a strong ETag from the response body itself.
//
// It is for a response whose bytes may change behind the same URL — a favicon
// replaced in place — where only the bytes themselves say when the picture
// changed. The cost is that the body has to be produced before the match can be
// checked; when the bytes are a pure function of an identifier, [ETagForKey]
// lets a match skip producing them at all.
func ETagForBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return etag(sum)
}

// ETagForKey derives a strong ETag from the identity of the bytes rather than
// the bytes: for a response that is a pure function of its key, such as a
// thumbnail a media server names with an id it changes whenever the artwork
// does. A match can then answer 304 before the upstream fetch and the decode,
// not merely before the transfer.
//
// Include a version of the pipeline that produces the bytes among the parts
// (an encoding constant), so a change to how they are made invalidates every
// client's copy. Parts are joined with a separator that cannot occur in them, so
// ("ab","c") and ("a","bc") never collide.
func ETagForKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return etag(sum)
}

// etag renders half the digest, quoted: 128 bits is far beyond collision
// concern for a cache validator, and the header stays short.
func etag(sum [sha256.Size]byte) string {
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// ETagMatches reports whether an If-None-Match header covers etag. The header is
// a comma-separated list, its entries may carry the weak-validator prefix, and
// `*` matches whatever the server has.
func ETagMatches(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// SendWithETag writes body with its validator and cache policy, answering 304
// Not Modified when the request's If-None-Match already covers etag. Both the
// 304 and the full response carry ETag and Cache-Control, so a client or an
// intermediary refreshes its copy's lifetime either way.
//
// A handler whose body is expensive to produce checks [ETagMatches] itself
// before producing it, and still ends with this call; the second check costs
// nothing.
func SendWithETag(c fiber.Ctx, etag, cacheControl, contentType string, body []byte) error {
	c.Set(fiber.HeaderETag, etag)
	c.Set(fiber.HeaderCacheControl, cacheControl)
	if ETagMatches(c.Get(fiber.HeaderIfNoneMatch), etag) {
		return c.SendStatus(fiber.StatusNotModified)
	}
	c.Set(fiber.HeaderContentType, contentType)
	return c.Send(body)
}
