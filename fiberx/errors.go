// Package fiberx holds cross-cutting Fiber v3 HTTP helpers: the error
// envelope, request binding/validation, metrics+logging middleware, rate
// limiting and optimistic-concurrency helpers. It never knows a caller's own
// domain sentinels — see DomainMapper for the seam that keeps them out.
package fiberx

import (
	"errors"
	"log/slog"

	"github.com/neodata-io/neokit/logx"

	"github.com/gofiber/fiber/v3"
)

// DomainMapper maps a caller's own error to an HTTP status, a public message
// and an error code. ok=false means "not mine" — the envelope then falls back
// to the generic status vocabulary. This is the seam that keeps a project's
// domain sentinels out of this package.
type DomainMapper func(err error) (status int, message, code string, ok bool)

// Errors renders the {"error","code","fields"} envelope, consulting an
// injected DomainMapper first for whatever error shapes a caller owns, then
// falling back to the generic vocabulary below. A nil mapper is fine — it
// simply means every error falls straight through to the generic path.
type Errors struct {
	mapper DomainMapper

	// Log receives request failures and summary records. Nil means
	// slog.Default(), preserving the standalone package's usual behavior.
	Log *slog.Logger

	// QuietPath reports whether a successful (non-4xx/5xx) request on the given
	// route template is pure noise that would drown the useful log lines — a
	// health check or a periodic status sweep a caller's own infrastructure polls
	// on a timer. Nil (the default) means MetricsAndLogger only silences the
	// structural cases (304, 101); a caller with noisy routes of its own sets
	// this once, right after NewErrors. This is the same seam as DomainMapper:
	// the route names a caller wants silenced are its own, not something this
	// package can know.
	QuietPath func(path string) bool
}

// NewErrors builds an Errors bound to m. m may be nil for a caller with no
// domain sentinels of its own (yet).
func NewErrors(m DomainMapper) *Errors {
	return &Errors{mapper: m}
}

// FieldError is one field-level validation failure, machine-readable so a client
// can map it to a form field instead of parsing a sentence: `field` is the JSON
// field name, `code` the stable validator slug (`required`, `email`, `min`, …),
// `message` a human string. It rides in APIError.Fields for a validation failure.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIError is the single carrier for an error response body. It serializes as
//
//	{"error": "<public message>", "code": "<stable slug>", "fields": [...]?}
//
// and implements error, so a handler can `return` it and the app's ErrorHandler
// (via Render) writes it. `error` stays a plain human string — the field every
// existing client already reads — for backward compatibility; `code` is the
// machine-stable discriminator; `fields` appears only for validation failures.
// The real cause (if any) is logged by whoever built the APIError, never
// serialized — the same rule the rest of this file enforces.
type APIError struct {
	Status  int          `json:"-"`
	Message string       `json:"error"`
	Code    string       `json:"code"`
	Fields  []FieldError `json:"fields,omitempty"`
}

func (e *APIError) Error() string { return e.Message }

// normalized fills in the fields a hand-written APIError is most likely to
// leave unset.
//
// Every field is exported and there is no constructor, so `&APIError{Message:
// "nope"}` is the obvious way to write one. A zero Status must therefore read as
// "the author didn't say" rather than as a status: c.Status(0) leaves fasthttp's
// default untouched, rendering an error body under HTTP 200.
func normalized(e *APIError) {
	if e.Status == 0 {
		e.Status = fiber.StatusInternalServerError
	}
	if e.Code == "" {
		e.Code = CodeForStatus(e.Status)
	}
}

// Unwrap exposes a *fiber.Error carrying the status and public message. Render,
// WriteError, and StatusForError all match *APIError first, so this is never the
// path that renders the full envelope (code + fields). It exists purely as a
// fallback: Fiber's own DefaultErrorHandler — the one a bare test app or any app
// that doesn't install Render uses — only understands *fiber.Error, and its
// asFiberError walks the Unwrap chain, so this keeps the correct status and public
// message even there instead of collapsing to a generic 500.
func (e *APIError) Unwrap() error { return fiber.NewError(e.Status, e.Message) }

// Render is the one place an error becomes a response body, so every path — a
// returned *APIError (validation), a bare *fiber.Error (a handler's own
// NewError, or Fail's return), or any other error — crosses the wire as the same
// {"error","code"} envelope. The app's ErrorHandler delegates here.
func (e *Errors) Render(c fiber.Ctx, err error) error {
	var ae *APIError
	if errors.As(err, &ae) {
		out := *ae // copy: rendering must not mutate the caller's error
		normalized(&out)
		return c.Status(out.Status).JSON(out)
	}
	var fe *fiber.Error
	if errors.As(err, &fe) {
		// A bare fiber.Error carries a deliberate status + public message but no
		// code; derive one from the status so the machine-readable field is never
		// absent. The cause (if any) was already logged by Fail; a hand-rolled
		// NewError has none to log.
		return c.Status(fe.Code).JSON(fiber.Map{"error": fe.Message, "code": CodeForStatus(fe.Code)})
	}
	return e.WriteError(c, err)
}

// WriteError maps an error to an appropriate HTTP status and machine code, and
// logs the real cause so the terse `http` summary line always has a companion
// "why" line joined by requestId. 4xx are the caller's fault (Warn); 5xx are
// ours (Error). Handlers that want to render an error inline call this
// directly; it writes the response and returns nil.
func (e *Errors) WriteError(c fiber.Ctx, err error) error {
	// An APIError already carries its own rendered shape (status/code/fields);
	// honor it rather than flattening it through the mapper/status switch.
	var ae *APIError
	if errors.As(err, &ae) {
		e.logCause(c, ae.Status, err)
		return c.Status(ae.Status).JSON(ae)
	}

	status, msg, code := e.mapError(err)
	e.logCause(c, status, err)
	return c.Status(status).JSON(fiber.Map{"error": msg, "code": code})
}

// Fail logs err's real cause (correlated by requestId, level chosen by status) and
// returns a *fiber.Error carrying only the public message, so the app's ErrorHandler
// renders {"error": public, "code": ...}. Use it wherever a handler needs a custom
// (e.g. localized) public message on a failure: bare fiber.NewError drops the cause,
// and the ErrorHandler only logs when it falls through to WriteError.
func (e *Errors) Fail(c fiber.Ctx, status int, public string, cause error) error {
	e.logCause(c, status, cause)
	return fiber.NewError(status, public)
}

// logCause emits the single correlated "why" line for a failure. A nil cause is a
// no-op (a client-supplied 4xx with nothing internal to explain).
func (e *Errors) logCause(c fiber.Ctx, status int, cause error) {
	if cause == nil {
		return
	}
	level := slog.LevelWarn
	if status >= 500 {
		level = slog.LevelError
	}
	// Correlated by requestId, which logx.ContextHandler stamps automatically from
	// the request context — the same id on the `http` summary line.
	e.logger().Log(c.Context(), level, "request failed",
		"status", status,
		"path", c.Path(),
		logx.Err(cause), // the mapped error's real cause, not the public message
	)
}

func (e *Errors) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// StatusForError returns the HTTP status the app's ErrorHandler will ultimately
// apply for a returned error: an *APIError or *fiber.Error carries its own code,
// anything else is mapped like WriteError does. The metrics/logging middleware
// needs this because it runs *before* the ErrorHandler writes the status, so
// reading c.Response().StatusCode() for a returned error would see the default 200.
func (e *Errors) StatusForError(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		out := *ae
		normalized(&out) // a zero Status must not be recorded as 0 in the metrics
		return out.Status
	}
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	status, _, _ := e.mapError(err)
	return status
}

// mapError translates an error into an HTTP status, the public
// (caller-facing) message, and a stable machine code. The full error is logged by
// WriteError; only the sanitized message + code cross the wire.
func (e *Errors) mapError(err error) (status int, msg, code string) {
	// A *fiber.Error anywhere in the chain already carries a deliberate status and
	// a public message, so it wins over the mapper below — including a cause it
	// wraps. That ordering is the point: a helper deep in the call stack can attach
	// one specific (and localized) message that every handler renders for free,
	// without the sentinel it wraps flattening it back to a generic string. It also
	// keeps this in step with StatusForError, which already reads fe.Code — before
	// this, the status on the log line and the status the client received could
	// disagree for the same error.
	var fe *fiber.Error
	if errors.As(err, &fe) {
		return fe.Code, fe.Message, CodeForStatus(fe.Code)
	}
	if e.mapper != nil {
		if status, msg, code, ok := e.mapper(err); ok {
			return status, msg, code
		}
	}
	return fiber.StatusInternalServerError, "internal server error", "internal"
}

// CodeForStatus gives a stable machine code for a response that carries no domain
// mapping — a bare fiber.Error or a Fail with only a status. A DomainMapper's own
// mappings win where they apply; this is the fallback so the `code` field is never
// absent.
func CodeForStatus(status int) string {
	switch status {
	case fiber.StatusBadRequest:
		return "bad_request"
	case fiber.StatusUnauthorized:
		return "unauthorized"
	case fiber.StatusForbidden:
		return "forbidden"
	case fiber.StatusNotFound:
		return "not_found"
	case fiber.StatusConflict:
		return "conflict"
	case fiber.StatusPreconditionFailed:
		return "precondition_failed"
	case fiber.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	case fiber.StatusUnprocessableEntity:
		return "unprocessable_entity"
	case fiber.StatusTooManyRequests:
		return "rate_limited"
	case fiber.StatusNotImplemented:
		return "not_implemented"
	case fiber.StatusBadGateway:
		return "bad_gateway"
	case fiber.StatusServiceUnavailable:
		return "unavailable"
	default:
		if status >= 500 {
			return "internal"
		}
		return "error"
	}
}
