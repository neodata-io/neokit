package httpc

import (
	"errors"
	"net/http"

	"github.com/neodata-io/neokit/errs"
)

// ErrNotFound is the shared sentinel an API client returns (typically wrapped)
// when a remote resource does not exist. Callers match it with errors.Is and
// [Classify] maps it to [FaultNotFound], so a client has to return this value
// rather than a private error of its own to participate in either contract.
//
// It is an alias of [github.com/neodata-io/neokit/errs.ErrNotFound], which makes
// "the upstream answered 404" and "this service has no such record" one value.
// The collapse is deliberate — it is already true of any service whose own
// domain sentinel reaches here — but it has an edge worth knowing: a handler
// that returns a raw httpc error from a failed outbound call renders 404 to its
// own client rather than 502. Wrap or map at that boundary if the distinction
// matters there.
var ErrNotFound = errs.ErrNotFound

// ErrAlreadyExists is the shared sentinel an API client returns (typically
// wrapped) when a resource cannot be created because one with the same identity
// already exists and cannot be safely adopted. Wrap it with a human-legible
// message ("username %q is already taken") so a caller can surface an actionable
// reason rather than a raw API error.
//
// [Classify] maps it to [FaultConflict], which is deliberately not retryable: a
// clash needs a human decision, never a retry that resolves it by silently
// rewriting the identity.
var ErrAlreadyExists = errors.New("already exists")

// IsConflict reports whether err is (or wraps) an HTTP 409 Conflict [APIError].
// It is how a client recognises that a create failed because the remote already
// has the resource, so it can wrap [ErrAlreadyExists] with a legible message
// instead of surfacing the raw API error.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}
