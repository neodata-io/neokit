package httpc

import (
	"errors"
	"net/http"
)

// ErrNotFound is the shared sentinel a plugin returns (typically wrapped) when a
// remote account or resource does not exist. The host matches it with
// errors.Is, so plugins must return this value rather than a private error to
// participate in that contract. A host's own domain package may re-export it
// under a domain-specific name for host-side use.
var ErrNotFound = errors.New("not found")

// ErrAlreadyExists is the shared sentinel a plugin returns (typically wrapped)
// when a remote account cannot be created because one with the same identity
// (username, email) already exists and cannot be safely adopted — e.g. the name
// is taken by an unrelated account. Wrap it with a human-legible message
// ("username %q is already taken") so the admin activity feed shows an
// actionable reason rather than a raw API error. The host matches it with
// errors.Is; provisioning treats it as a normal step failure (the user lands in
// Failed and the admin resolves the clash, e.g. by renaming, then retries just
// that service) — it is never auto-resolved by silently rewriting the identity.
var ErrAlreadyExists = errors.New("already exists")

// IsConflict reports whether err is (or wraps) an HTTP 409 Conflict [APIError].
// It is the shared way for a plugin to recognise that a create failed because
// the remote already has the resource, so it can wrap [ErrAlreadyExists] with a
// legible message instead of surfacing the raw API error.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}
