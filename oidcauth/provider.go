package oidcauth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/neodata-io/neokit/httpc"
)

// Provider is the relying party. Construct it with [New]; it is safe for
// concurrent use and holds no per-login state.
type Provider struct {
	cfg  Config
	http *http.Client

	// Discovery is fetched once and reused. The mutex is held across the network
	// call so a burst of logins on a cold start makes one request rather than N.
	// Login is rare and bounded by ctx, so the contention does not matter.
	mu       sync.Mutex
	provider *gooidc.Provider
}

// New builds a provider, declining (ok=false) unless [Config.Configured] holds.
//
// It performs **no I/O**: discovery happens lazily on first use. That is what
// makes it safe to call during startup wiring, on every configuration save, and
// on a throwaway "test connection" — none of which should be able to block on a
// provider that is slow or down.
//
// The declining return is the idiom for an optional login gate:
//
//	authn, ok := oidcauth.New(cfg)   // ok == false ⇒ no login configured
func New(cfg Config) (*Provider, bool) {
	cfg.Normalize()
	if !cfg.Configured() {
		return nil, false
	}
	client := cfg.HTTPClient
	if client == nil {
		client = httpc.NewHTTPClient(httpc.HTTPOptions{Timeout: DefaultHTTPTimeout})
	}
	return &Provider{cfg: cfg, http: client}, true
}

// Config returns the normalized configuration. The client secret is included —
// it is the caller's own value — so do not log the result.
func (p *Provider) Config() Config { return p.cfg }

// Issuer is the configured provider URL.
func (p *Provider) Issuer() string { return p.cfg.Issuer }

// RedirectURI is the absolute callback URL that must be registered with the
// provider as an allowed redirect.
func (p *Provider) RedirectURI() string { return p.cfg.BaseURL + p.cfg.CallbackPath }

// PostLogoutURI is where the provider returns the browser after an RP-initiated
// logout. Providers generally require this to be registered too.
func (p *Provider) PostLogoutURI() string { return p.cfg.BaseURL + p.cfg.PostLogoutPath }

// CookieSecure reports whether this deployment is HTTPS, and therefore whether
// auth cookies must carry the Secure attribute.
//
// It reads the configured BaseURL, never the request: behind a TLS-terminating
// proxy the request arrives over plain HTTP, so scheme-sniffing would drop
// Secure on exactly the deployments that need it. See [Config.BaseURL].
func (p *Provider) CookieSecure() bool {
	u, err := url.Parse(p.cfg.BaseURL)
	return err == nil && u.Scheme == "https"
}

// discover resolves and caches the provider metadata.
func (p *Provider) discover(ctx context.Context) (*gooidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, nil
	}
	prov, err := gooidc.NewProvider(p.clientCtx(ctx), p.cfg.Issuer)
	if err != nil {
		return nil, discoveryErr(err)
	}
	p.provider = prov
	return prov, nil
}

// clientCtx injects our retrying, traced client into both libraries. A bare
// client here would silently lose the retry transport and the otel spans.
func (p *Provider) clientCtx(ctx context.Context) context.Context {
	return gooidc.ClientContext(context.WithValue(ctx, oauth2.HTTPClient, p.http), p.http)
}

func (p *Provider) oauthConfig(prov *gooidc.Provider) oauth2.Config {
	return oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     prov.Endpoint(),
		RedirectURL:  p.RedirectURI(),
		Scopes:       p.cfg.Scopes,
	}
}

// AuthorizeURL is where the browser is sent to log in.
//
// state, nonce and verifier are the caller's three per-login secrets: state is
// the CSRF double-submit, nonce binds the ID token to this handshake, and
// verifier is the PKCE verifier. Generate them with [NewHandshake] and keep them
// somewhere only this browser can return — never in state itself, which crosses
// two other origins and comes back in a URL.
//
// The verifier never travels: only its S256 challenge is sent.
func (p *Provider) AuthorizeURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	prov, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	cfg := p.oauthConfig(prov)
	return cfg.AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	), nil
}

// Exchange trades the authorization code for a verified [Identity].
//
// It performs the full relying-party check: the code is exchanged with the PKCE
// verifier, the ID token's signature, issuer, audience and expiry are verified
// against the provider's JWKS, and the nonce is compared against the one this
// login started with. Every failure returns one of the package sentinels, never
// the provider's response body.
func (p *Provider) Exchange(ctx context.Context, code, nonce, verifier string) (Identity, error) {
	prov, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	cfg := p.oauthConfig(prov)

	tok, err := cfg.Exchange(p.clientCtx(ctx), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, exchangeErr(err)
	}

	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return Identity{}, fmt.Errorf("%w: the provider returned no id_token", ErrTokenInvalid)
	}

	idToken, err := prov.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID}).Verify(p.clientCtx(ctx), rawID)
	if err != nil {
		// Signature, issuer, audience and expiry all fail here. Do NOT %w-wrap err:
		// a failed JWKS fetch renders as "get keys failed: <status> <body>", which
		// would put an upstream body into an admin-facing log.
		//
		// Expiry is worth naming because it is usually nobody's fault — a drifted
		// container clock rejects every token, and "check the clock" is the whole
		// fix. TokenExpiredError is typed and body-free, so lifting it is safe.
		var expired *gooidc.TokenExpiredError
		if errors.As(err, &expired) {
			return Identity{}, fmt.Errorf("%w: the id_token was already expired on arrival — check the server clock", ErrTokenInvalid)
		}
		return Identity{}, ErrTokenInvalid
	}

	// Verify does NOT check the nonce; OIDC Core §3.1.3.7 makes that the relying
	// party's job. Without this an attacker can replay an id_token minted for a
	// different login.
	//
	// Both sides are rejected when blank, *before* they are compared: a token with
	// no nonce claim against a caller that lost its nonce would otherwise match as
	// "" == "" and pass a check that never actually ran.
	if nonce == "" || idToken.Nonce == "" || idToken.Nonce != nonce {
		return Identity{}, fmt.Errorf("%w: id_token nonce mismatch", ErrTokenInvalid)
	}

	return p.identityFrom(idToken)
}

// identityFrom maps verified claims onto an Identity.
func (p *Provider) identityFrom(idToken *gooidc.IDToken) (Identity, error) {
	// Claims are read into a map rather than a struct because the groups claim is
	// configurable: providers put membership under "groups", "roles", or a
	// namespaced URI, and a fixed struct tag cannot follow that.
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: unreadable id_token claims", ErrTokenInvalid)
	}

	id := Identity{
		Subject: strings.TrimSpace(stringClaim(claims, "sub")),
		Name:    strings.TrimSpace(stringClaim(claims, "name")),
		Email:   strings.TrimSpace(stringClaim(claims, "email")),
		Groups:  stringsClaim(claims, p.cfg.GroupsClaim),
	}
	if id.Subject == "" {
		return Identity{}, fmt.Errorf("%w: id_token carries no subject", ErrTokenInvalid)
	}
	if id.Name == "" {
		// A display name is optional in OIDC; falling back to the subject keeps
		// every consumer free of "" handling.
		id.Name = id.Subject
	}
	id.Owner = p.IsOwner(id.Groups)
	return id, nil
}

// IsOwner reports whether a group list confers ownership: any identity when no
// owner group is configured, else membership in that group (case-insensitive).
//
// Exported because an application that re-derives authorization later — after a
// group list changed at the provider — must use the same rule this package
// applied at login, not a second copy of it.
func (p *Provider) IsOwner(groups []string) bool {
	if p.cfg.OwnerGroup == "" {
		return true
	}
	for _, g := range groups {
		if strings.EqualFold(strings.TrimSpace(g), p.cfg.OwnerGroup) {
			return true
		}
	}
	return false
}

// stringClaim reads a string claim, tolerating its absence.
func stringClaim(claims map[string]any, key string) string {
	s, _ := claims[key].(string)
	return s
}

// stringsClaim reads a list-of-strings claim.
//
// It accepts both encodings that appear in the wild: a JSON array (the common
// case) and a single string (some providers emit a lone group unwrapped). A
// non-string element is skipped rather than failing the login — losing one group
// costs an authorization decision, while rejecting the token costs the sign-in
// entirely.
func stringsClaim(claims map[string]any, key string) []string {
	switch v := claims[key].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return v
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	default:
		return nil
	}
}

// EndSessionURL is where the browser goes to sign out *at the provider*, or ""
// when the provider advertises no end-session endpoint.
//
// Without it, signing out drops only the local session: the provider's stays, so
// the next sign-in completes silently with no prompt and the "sign out" button
// is, on a shared device, a lie.
//
// It sends client_id rather than id_token_hint. Both are permitted by OpenID
// Connect RP-Initiated Logout 1.0 §2, and the alternative means keeping the raw
// id_token at rest for the lifetime of every session purely to hand it back — a
// credential stored for nothing, since client_id already tells the provider
// which relying party is asking.
//
// An absent endpoint is ("", nil), not an error: plenty of providers simply do
// not implement RP-initiated logout, and the caller's local sign-out is complete
// on its own.
func (p *Provider) EndSessionURL(ctx context.Context) (string, error) {
	prov, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	// end_session_endpoint is not part of go-oidc's Provider struct, so it comes
	// out of the raw discovery document.
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := prov.Claims(&meta); err != nil || strings.TrimSpace(meta.EndSession) == "" {
		return "", nil
	}
	u, err := url.Parse(meta.EndSession)
	if err != nil {
		return "", nil
	}
	q := u.Query()
	q.Set("client_id", p.cfg.ClientID)
	q.Set("post_logout_redirect_uri", p.PostLogoutURI())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// CheckHealth proves the issuer is reachable and serving discovery, **and** that
// the configured client id and secret authenticate against the token endpoint.
// It bypasses the discovery cache, so it reflects the network now rather than at
// first login.
//
// The credential half matters: discovery is unauthenticated, so on its own it
// waves a mistyped secret through and the gate then closes on a login that can
// never complete.
func (p *Provider) CheckHealth(ctx context.Context) error {
	prov, err := gooidc.NewProvider(p.clientCtx(ctx), p.cfg.Issuer)
	if err != nil {
		return discoveryErr(err)
	}
	return p.verifyClientCredentials(ctx, prov)
}

// verifyClientCredentials asks the token endpoint to reject a deliberately bogus
// authorization code, and reads *which* rejection comes back.
//
// The trick is that a token request carrying a wrong client secret fails
// differently from one carrying a bad code: RFC 6749 §5.2 defines
// `invalid_client` for the former (also surfaced as a 401) and `invalid_grant`
// for the latter. So a request we already know will be rejected still reports
// whether the *credentials* were accepted, without ever completing a real login.
//
// It fails only on a definite credential rejection. Anything else — including a
// provider that answers in some non-standard way — is treated as "not disproven",
// so this can never block a legitimate setup, only catch an unambiguous typo.
func (p *Provider) verifyClientCredentials(ctx context.Context, prov *gooidc.Provider) error {
	cfg := p.oauthConfig(prov)
	_, err := cfg.Exchange(p.clientCtx(ctx), "oidcauth-credential-probe-not-a-real-code")
	if err == nil {
		return nil // absurd, but a success is not a credential failure
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if clientWasRejected(re) {
			// The sentinel, not the raw error: its body can carry provider detail.
			return ErrClientRejected
		}
		// Any other RFC 6749 error means the credentials were accepted and only
		// the bogus code was refused — exactly what we want to see.
		return nil
	}
	// A transport failure here is a network problem, not a credential one;
	// discovery already passed, so don't fail the config over a blip.
	return nil
}

// discoveryErr strips the upstream response body out of a discovery failure and
// tags the two causes an operator can act on.
//
// [httpc.Redact] is not enough on its own: it rewrites a *url.Error (where auth
// can ride in the URL) and returns anything else untouched. go-oidc builds this
// error with fmt.Errorf("%s: %s", resp.Status, body), which is not a *url.Error —
// so `%w`-wrapping it would put the upstream body verbatim into an admin-facing
// message.
//
// The context sentinels are deliberately preserved: [httpc.Classify] maps them to
// FaultUnavailable, which is how a caller tells "the network is down" from "your
// configuration is wrong".
func discoveryErr(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrDiscoveryFailed, err)
	}
	// A name that will not resolve is the single most common containerized-OIDC
	// failure: the container's resolver has no record for the public issuer host.
	// The DNS failure never reached the service, so it carries no response body —
	// nothing to redact — and wrapping the *net.DNSError keeps the cause
	// credential-free while Classify still maps net.Error to FaultUnavailable.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("%w: %w", ErrUnresolved, dnsErr)
	}
	// A reachable issuer whose certificate does not verify is a distinct,
	// actionable failure — "use a trusted certificate", not "unreachable". A cert
	// error carries no response body either.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return fmt.Errorf("%w: %w", ErrUntrusted, certErr)
	}
	// A transport failure carries no response body, but its URL may carry
	// credentials — precisely Redact's case.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%w: %w", ErrDiscoveryFailed, httpc.Redact(err))
	}
	// Anything else is go-oidc's own "status: body". Keep the diagnosis, drop the
	// payload.
	return fmt.Errorf("%w: the provider did not return a valid discovery document", ErrDiscoveryFailed)
}

// tokenEndpointErrors is RFC 6749 §5.2's registered set of token-endpoint error
// codes. It is a closed allow-list rather than a validity check: `error` is
// provider-controlled bytes on the wire, so the only way to name it in a log
// without reopening the body leak this file works to close is to echo nothing
// that is not already one of ours.
var tokenEndpointErrors = map[string]bool{
	"invalid_request":        true,
	"invalid_client":         true,
	"invalid_grant":          true,
	"unauthorized_client":    true,
	"unsupported_grant_type": true,
	"invalid_scope":          true,
}

// clientWasRejected reports whether a token-endpoint rejection was aimed at the
// client credentials rather than at the grant.
//
// An explicit code wins over the status: a provider that answers 401 alongside a
// named `invalid_grant` is describing the code, and reading the status first is
// how a stale code gets misreported as a bad secret. Both the login exchange and
// the health probe classify through here so they cannot disagree.
func clientWasRejected(re *oauth2.RetrieveError) bool {
	if re.ErrorCode != "" {
		return re.ErrorCode == "invalid_client"
	}
	return re.Response != nil && re.Response.StatusCode == http.StatusUnauthorized
}

// exchangeErr turns a failed code exchange into a sentinel, keeping the one
// diagnostic worth having — which RFC 6749 code came back — and dropping
// everything that could carry a credential.
func exchangeErr(err error) error {
	// No RFC 6749 answer at all means nothing rejected anything: the request was
	// refused, timed out, or failed TLS. oauth2 returns *RetrieveError for every
	// non-2xx, so its absence is decisive.
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		// A transport failure carries no response body, but its URL may carry
		// credentials — Redact's case, and it preserves the wrapped cause so a
		// cancelled or timed-out request still matches its context sentinel.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("%w: %w", ErrTokenEndpointUnreachable, httpc.Redact(err))
		}
		// What is left is oauth2's own parse failure over a 2xx body ("cannot parse
		// json: …"), whose message can quote a fragment of that body. Keep the
		// diagnosis, drop the payload.
		return fmt.Errorf("%w: the token endpoint returned a response that could not be parsed", ErrTokenEndpointUnreachable)
	}

	// Never re.Error(): it renders error_description, and with no code the raw
	// body. Only a code we already recognise is echoed.
	answered := "an unrecognised error"
	if tokenEndpointErrors[re.ErrorCode] {
		answered = re.ErrorCode
	}
	if clientWasRejected(re) {
		return fmt.Errorf("%w (token endpoint answered %s)", ErrClientRejected, answered)
	}
	return fmt.Errorf("%w (token endpoint answered %s)", ErrCodeRejected, answered)
}
