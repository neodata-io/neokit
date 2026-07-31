// Package oidcauth is a provider-agnostic OpenID Connect relying party: the
// half of a login that talks to an identity provider. It speaks nothing but
// standard OIDC, so it works with Pocket ID, Authentik, Keycloak, Zitadel,
// Authelia, Okta, Entra, or anything else certified.
//
// It is transport-free on purpose. This package owns discovery, the authorize
// URL, the code exchange and ID-token verification; it never sees an HTTP
// request, sets no cookies, and stores nothing. The browser-facing half — routes,
// cookies, session middleware — lives in [github.com/neodata-io/neokit/oidcauth/fiberauth],
// so an application on a different HTTP stack can reuse everything here and write
// only the small part that is framework-shaped.
//
// # Secure by default
//
// The defaults are the strict reading of the specs, and there is no knob to relax
// them: PKCE S256 on every authorization request, a nonce checked on every ID
// token (OIDC Core §3.1.3.7 requires the *relying party* to do this — the token
// verifier will not), the audience pinned to the client id, and the redirect URI
// derived from configuration rather than from a request header a caller can spoof.
//
// # Errors never carry an upstream body
//
// Every failure path in this package deliberately drops the provider's response
// body and returns one of the sentinels in errors.go instead. That is not
// tidiness. A discovery URL typo'd onto an internal service echoes that service's
// body back to whoever pressed the button; a token endpoint renders
// `error_description` as provider-controlled free text; a failed JWKS fetch is
// rendered by the underlying library as "get keys failed: <status> <body>". Each
// one puts bytes you do not control into an admin-facing message or a log. The
// sentinels carry the diagnosis; the body is dropped.
package oidcauth

import (
	"net/http"
	"strings"
	"time"
)

// Default scopes. openid is mandatory; profile and email fill the display name
// and address; groups is where most self-hosted providers put group membership,
// and is what [Config.OwnerGroup] reads.
//
// A provider that does not know the groups scope may reject the whole request —
// override [Config.Scopes] there.
var DefaultScopes = []string{"openid", "profile", "email", "groups"}

// Defaults for the paths and claim names, all overridable on [Config].
const (
	DefaultCallbackPath   = "/api/auth/callback"
	DefaultPostLogoutPath = "/"
	DefaultGroupsClaim    = "groups"
)

// DefaultHTTPTimeout bounds a single call to the provider (discovery, the token
// exchange, a JWKS fetch). A login is interactive: a user is watching a blank
// tab, so the budget is short by design.
const DefaultHTTPTimeout = 15 * time.Second

// Config describes the provider and the client registered with it.
//
// The four required fields are Issuer, ClientID, ClientSecret and BaseURL. Leave
// any of them blank and [New] declines — which is how an application makes its
// login gate optional without a second "enabled" flag that can disagree with the
// credentials.
type Config struct {
	// Issuer is the provider's base URL, the value its discovery document
	// publishes as `issuer` (e.g. "https://id.example.com").
	Issuer string
	// ClientID and ClientSecret are the credentials registered with the provider.
	ClientID     string
	ClientSecret string

	// BaseURL is *this application's* own public origin (e.g.
	// "https://app.example.com"). The redirect URI is BaseURL+CallbackPath.
	//
	// It comes from configuration rather than from the request's Host header on
	// purpose: Host is attacker-controlled, and a redirect URI derived from it is
	// how an open redirect becomes a token leak. It is also what
	// [Provider.CookieSecure] reads, so the session cookie's Secure flag is right
	// behind a TLS-terminating reverse proxy — where the request itself arrives
	// over plain HTTP and every scheme-sniffing heuristic gets it wrong.
	BaseURL string

	// OwnerGroup gates administrative access: an identity is [Identity.Owner]
	// when it is a member of this group (compared case-insensitively).
	//
	// Blank means **every authenticated user is an owner**. That is the right
	// default for a single-admin deployment and the wrong one for anything else,
	// so set it whenever more than one person can sign in at the provider.
	OwnerGroup string

	// Scopes overrides [DefaultScopes].
	Scopes []string

	// GroupsClaim is the ID-token claim holding group membership. Defaults to
	// [DefaultGroupsClaim]. Providers vary — Entra uses "roles", some deployments
	// use a namespaced URI.
	GroupsClaim string

	// CallbackPath is the path component of the redirect URI, appended to
	// BaseURL. Defaults to [DefaultCallbackPath].
	//
	// Changing it means re-registering the redirect URI at the provider, so it is
	// the one setting that cannot be changed unilaterally.
	CallbackPath string

	// PostLogoutPath is where the provider returns the browser after an
	// RP-initiated logout, appended to BaseURL. Defaults to [DefaultPostLogoutPath].
	PostLogoutPath string

	// HTTPClient talks to the provider. Nil builds one through neokit/httpc with
	// [DefaultHTTPTimeout] — which carries the retry transport and, with it, the
	// otel spans a hand-rolled client silently loses.
	HTTPClient *http.Client
}

// Normalize trims every value and strips the trailing slash from the two URLs,
// so a pasted "https://id.example.com/" and "https://id.example.com" resolve to
// the same provider and produce the same redirect URI. It also fills the
// defaults, so a normalized Config reads back exactly what [New] will use.
func (c *Config) Normalize() {
	c.Issuer = strings.TrimRight(strings.TrimSpace(c.Issuer), "/")
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.OwnerGroup = strings.TrimSpace(c.OwnerGroup)

	if len(c.Scopes) == 0 {
		c.Scopes = DefaultScopes
	}
	if c.GroupsClaim = strings.TrimSpace(c.GroupsClaim); c.GroupsClaim == "" {
		c.GroupsClaim = DefaultGroupsClaim
	}
	c.CallbackPath = normalizePath(c.CallbackPath, DefaultCallbackPath)
	c.PostLogoutPath = normalizePath(c.PostLogoutPath, DefaultPostLogoutPath)
}

// normalizePath fills a blank path with its default and guarantees a leading
// slash, so BaseURL+path is always a well-formed URL however it was written.
func normalizePath(p, def string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return def
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// Configured reports whether all four required fields are present — the same
// condition under which [New] returns a provider. OwnerGroup is optional.
func (c Config) Configured() bool {
	return strings.TrimSpace(c.Issuer) != "" &&
		strings.TrimSpace(c.ClientID) != "" &&
		strings.TrimSpace(c.ClientSecret) != "" &&
		strings.TrimSpace(c.BaseURL) != ""
}
