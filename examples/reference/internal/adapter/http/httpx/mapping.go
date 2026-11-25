// Package httpx is what the HTTP modules share and none of them owns. It is the
// only package under adapter/http that a module may import besides its own.
package httpx

import (
	"errors"
	"net/http"

	"github.com/neodata-io/neokit/examples/reference/internal/domain"
)

// MapError is the service's half of the error envelope: which of its own
// sentinels is which status, public sentence and stable code. The composition
// root hands it to the app, so a handler returns an error and never renders
// one.
//
// ErrNotFound, ErrInvalidInput and ErrConflict are deliberately absent. They
// are neokit's own values, and fiberx renders them without being told — a case
// here would be a second copy of that mapping, free to drift. Add a case only
// for a sentinel that is genuinely this service's.
//
// Returning ok=false means "not mine", and anything nobody claims is a 500
// whose cause is logged and never sent.
func MapError(err error) (status int, message, code string, ok bool) {
	switch {
	case errors.Is(err, domain.ErrStale):
		return http.StatusPreconditionFailed,
			"the task was changed since you loaded it; reload and try again", "stale", true
	default:
		return 0, "", "", false
	}
}
