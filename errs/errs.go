// Package errs holds the error sentinels neokit's HTTP layer recognises without
// being told. A service returns one of these from its domain layer and
// [github.com/neodata-io/neokit/fiberx] renders the right status and code with
// no DomainMapper of its own.
//
// This is a leaf package with no dependencies beyond the standard library, and
// deliberately so: a domain layer must be able to return these without importing
// Fiber.
//
// The set is small on purpose. These three are the ones a service cannot avoid
// needing; anything narrower belongs in that service's own mapper, which fiberx
// consults first and which always wins.
package errs

import "errors"

var (
	// ErrNotFound is "there is no such record". It renders as 404 / "not_found".
	//
	// [github.com/neodata-io/neokit/httpc.ErrNotFound] is an alias of this value,
	// so an outbound client's 404 and a local miss are one sentinel. That is a
	// deliberate collapse — see the httpc doc comment for what it costs.
	ErrNotFound = errors.New("not found")

	// ErrInvalidInput is "the caller sent something unusable" for a reason no
	// field-level validator expressed. It renders as 400 / "invalid_input".
	// Field-level failures should be a fiberx.APIError carrying Fields instead,
	// so the client can attach each message to its own input.
	ErrInvalidInput = errors.New("invalid input")

	// ErrConflict is "the write clashed with another one" — an optimistic
	// concurrency failure or a uniqueness clash. It renders as 409 / "conflict".
	ErrConflict = errors.New("conflict")
)
