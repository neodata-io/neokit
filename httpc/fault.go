package httpc

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// Fault classifies *why* a call failed, so a caller can decide what to do about
// it: "unhealthy" is true and useless — it says nothing about whether to fix a
// password or wait five minutes, to back off or give up.
//
// It is a closed, small set on purpose. The discipline is that a Fault must
// change what a caller does; if two faults lead to the same decision, they are
// one fault.
type Fault string

const (
	// FaultNone is the classification of a nil error.
	FaultNone Fault = ""

	// FaultAuth: the credentials are wrong, expired, or lack permission (401, 403).
	// The admin has to act. Retrying will not help and only burns rate limit.
	FaultAuth Fault = "auth"

	// FaultNotFound: the resource does not exist (404, or a wrapped ErrNotFound).
	// Often a normal answer rather than a failure — "this user has no account here".
	FaultNotFound Fault = "not_found"

	// FaultConflict: it already exists and cannot be safely adopted (409, or a
	// wrapped ErrAlreadyExists). Needs a human decision, not a retry.
	FaultConflict Fault = "conflict"

	// FaultRateLimited: too many requests (429). Back off. Notably NOT a service
	// outage — reporting it as one is how a monitor learns to cry wolf.
	FaultRateLimited Fault = "rate_limited"

	// FaultUnavailable: the service is down, unreachable, or timed out (5xx, a dial
	// error, DNS, a deadline). Transient by assumption. Wait; do not page an admin
	// who can do nothing about it.
	FaultUnavailable Fault = "unavailable"

	// FaultUnsupported: the service cannot do this — the endpoint is absent on this
	// version, or the feature is not enabled (404 on a *feature*, 405, 501). A
	// permanent no, so a caller should stop asking rather than retry forever.
	FaultUnsupported Fault = "unsupported"

	// FaultUnknown: an error the SDK cannot place. Deliberately distinct from
	// FaultNone (no error) and from FaultUnavailable (a *claim* that it is
	// transient): guessing "transient" for an error we do not understand is how a
	// permanent failure gets retried forever in silence.
	FaultUnknown Fault = "unknown"
)

// Retryable reports whether trying again could plausibly succeed without anyone
// intervening. Schedulers use it to decide between backing off and giving up.
//
// FaultUnknown is not retryable, and that is the conservative choice on purpose: an
// error nobody has classified is more likely a bug than a blip, and retrying it
// forever hides it.
func (f Fault) Retryable() bool {
	return f == FaultRateLimited || f == FaultUnavailable
}

// AdminActionable reports whether this is something the operator can actually fix —
// which is the difference between a badge worth surfacing and noise. A wrong API
// key is worth telling someone about; a NAS that is briefly down is not.
func (f Fault) AdminActionable() bool {
	return f == FaultAuth || f == FaultConflict
}

// Classify derives a Fault from an error by walking the chain: the shared sentinels
// first, then an [APIError]'s status, then the network/context error kinds.
//
// It works on any client that returns a well-formed error — which is exactly why
// [BaseClient] and [CheckStatus] both produce an *APIError instead of a formatted
// string. A client that stringifies its status ("myservice: HTTP 401") lands here
// as FaultUnknown, and its operator gets "unhealthy" instead of "check your API key".
func Classify(err error) Fault {
	if err == nil {
		return FaultNone
	}

	// Sentinels first: a client that means "no such resource" says so explicitly, and
	// that is more precise than any status code it happened to arrive on.
	switch {
	case errors.Is(err, ErrNotFound):
		return FaultNotFound
	case errors.Is(err, ErrAlreadyExists):
		return FaultConflict
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return faultForStatus(apiErr.StatusCode)
	}

	// A cancelled context is us giving up (shutdown, a caller's deadline), not the
	// service failing. Reporting it as an outage would fill the log with alarms on
	// every clean restart.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return FaultUnavailable
	}

	// Anything that never reached the service: a dial refusal, DNS, a TLS failure, a
	// transport timeout.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return FaultUnavailable
	}

	return FaultUnknown
}

func faultForStatus(status int) Fault {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return FaultAuth
	case status == http.StatusNotFound:
		return FaultNotFound
	case status == http.StatusConflict:
		return FaultConflict
	case status == http.StatusTooManyRequests:
		return FaultRateLimited
	case status == http.StatusMethodNotAllowed, status == http.StatusNotImplemented:
		return FaultUnsupported
	case status >= 500:
		return FaultUnavailable
	}
	return FaultUnknown
}
