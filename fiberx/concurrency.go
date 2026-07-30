package fiberx

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// This file exposes a store's optimistic-concurrency counter to clients as a
// conditional-request contract. A mutable single resource stamps a weak ETag
// derived from its version on reads; a conditional
// write sends `If-Match: <etag>` and gets 412 if the resource moved on since,
// instead of silently clobbering a concurrent edit. Absence of If-Match is not a
// failure — the precondition is opt-in, so existing callers never regress.

// ETagForVersion renders an optimistic-concurrency version as a weak ETag. Weak
// (W/"…") because it validates the logical resource version, not a byte-for-byte
// body: two reads at the same version may differ in a derived field (a relative
// timestamp) yet represent the same editable state.
func ETagForVersion(version int) string {
	return fmt.Sprintf(`W/"%d"`, version)
}

// SetVersionETag stamps the weak ETag for a version onto the response, so the
// client can echo it back as If-Match on its next write.
func SetVersionETag(c fiber.Ctx, version int) {
	c.Set(fiber.HeaderETag, ETagForVersion(version))
}

// CheckIfMatch enforces an If-Match precondition against the resource's current
// version. It returns nil to proceed — when the client sent no If-Match (the
// precondition is opt-in, absence is not a failure) or the supplied validator
// matches the current version (or is `*`) — and a 412 *fiber.Error otherwise, so a
// stale editor is told to reload rather than overwrite a change it never saw. Both
// the weak (`W/"3"`) and strong (`"3"`) forms are accepted, and the header may be
// a comma-separated list.
func CheckIfMatch(c fiber.Ctx, currentVersion int) error {
	header := strings.TrimSpace(c.Get(fiber.HeaderIfMatch))
	if header == "" {
		return nil
	}
	want := strconv.Itoa(currentVersion)
	for _, cand := range strings.Split(header, ",") {
		cand = strings.TrimSpace(cand)
		if cand == "*" {
			return nil
		}
		cand = strings.TrimPrefix(cand, "W/")
		cand = strings.Trim(cand, `"`)
		if cand == want {
			return nil
		}
	}
	return fiber.NewError(fiber.StatusPreconditionFailed,
		"the resource was modified since you loaded it; reload and retry")
}

// IfMatchVersion parses the If-Match header into the version a conditional write
// must match, for resources that carry a plain integer version. conditional is
// false when no usable precondition was sent (the header is absent, or is `*` =
// match any), so the caller does an unconditional write; true with the parsed
// version otherwise. An unparseable value yields version -1 (conditional true), so
// it can never accidentally match a real version — it fails the precondition
// rather than silently passing it.
func IfMatchVersion(c fiber.Ctx) (version int, conditional bool) {
	header := strings.TrimSpace(c.Get(fiber.HeaderIfMatch))
	if header == "" {
		return 0, false
	}
	first := strings.TrimSpace(strings.SplitN(header, ",", 2)[0])
	if first == "*" {
		return 0, false
	}
	first = strings.Trim(strings.TrimPrefix(first, "W/"), `"`)
	v, err := strconv.Atoi(first)
	if err != nil {
		return -1, true
	}
	return v, true
}
