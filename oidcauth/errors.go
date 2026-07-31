package oidcauth

import "errors"

// The closed set of reasons a login or a health check can fail.
//
// They are sentinels rather than error strings because every consumer needs to
// branch on *which* thing is wrong, and each one has a different fix belonging to
// a different person: a name that will not resolve is the deployment's, an
// untrusted certificate is the reverse proxy's, a rejected client is a
// copy-paste, a rejected code is nobody's and simply needs a retry. Matching with
// errors.Is means a caller can render a localised, actionable message without
// parsing English — and the underlying cause still reaches the logs.
//
// The split between [ErrClientRejected] and [ErrCodeRejected] is the one worth
// reading twice. They share a moment in the handshake and nothing else. Folded
// together — as they were in the implementation this was extracted from — a code
// that had merely gone stale sent an admin off to re-copy a client secret that
// was never wrong.
var (
	// ErrUnresolved: the issuer host did not resolve. In a container this is
	// almost always the resolver rather than a typo — split-horizon DNS with no
	// record for the public issuer name.
	ErrUnresolved = errors.New("oidc: issuer host did not resolve")

	// ErrUntrusted: the issuer was reachable but its TLS certificate did not
	// verify — typically a reverse proxy serving from an internal CA.
	ErrUntrusted = errors.New("oidc: issuer certificate not trusted")

	// ErrDiscoveryFailed: the issuer answered, but not with a usable discovery
	// document. Its response body is deliberately not attached.
	ErrDiscoveryFailed = errors.New("oidc: provider discovery failed")

	// ErrClientRejected: the token endpoint rejected the client id or secret —
	// RFC 6749 §5.2's `invalid_client`, and the bare 401 the spec reserves for a
	// failed client authentication.
	ErrClientRejected = errors.New("oidc: provider rejected the client id or secret")

	// ErrCodeRejected: the token endpoint refused the authorization *grant* —
	// `invalid_grant` and its neighbours. Overwhelmingly a code that was already
	// spent (a refreshed callback tab) or that sat unspent past its very short
	// life. Neither is a misconfiguration.
	ErrCodeRejected = errors.New("oidc: provider rejected the authorization code")

	// ErrTokenEndpointUnreachable: the token request never got an answer —
	// refused, timed out, or failed TLS. Discovery had already succeeded, so the
	// issuer itself resolves; it is the exchange that could not complete.
	//
	// It is separate because the alternative is an assertion about a conversation
	// that never happened: folded into ErrCodeRejected, a provider that was merely
	// *down* was reported as having rejected a code it never received.
	ErrTokenEndpointUnreachable = errors.New("oidc: token endpoint could not be reached")

	// ErrTokenInvalid: the id_token was absent, failed verification, or carried
	// the wrong nonce or no subject. Signature, issuer, audience and expiry all
	// land here — and a container whose clock has drifted is the most common
	// benign cause, which is why [Provider.Exchange] names it.
	ErrTokenInvalid = errors.New("oidc: id_token failed verification")

	// ErrNotConfigured: a Provider method was reached with no usable
	// configuration. Callers normally prevent this by checking [New]'s second
	// return value.
	ErrNotConfigured = errors.New("oidc: provider is not configured")
)
