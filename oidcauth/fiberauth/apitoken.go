package fiberauth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/apitoken"
	"github.com/neodata-io/neokit/errs"
	"github.com/neodata-io/neokit/fiberx"
	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/oidcauth"
)

// Personal API tokens: how a script, a CI job or a native app acts as a
// signed-in person. Enabled by [Options.Tokens]; without it nothing in this file
// is mounted.
//
// The model is "a token may do what its maker may, except what takes a
// browser". A token is recognised wherever a session would be, so guards and
// handlers do not know the difference — and the routes that could turn one
// leaked token into a lasting foothold refuse it with [RefuseAPIToken]: making
// more tokens, and whatever an application adds to that list (changing the
// login, reading stored credentials, downloading a backup).

// localTokenKey marks a request as authenticated by a token, holding the token's
// row id. Unexported, so nothing outside this package can set it.
const localTokenKey = "neokit_api_token_id"

// ErrTokenNotAllowed answers a token on a route that wants a browser session.
var ErrTokenNotAllowed = &fiberx.APIError{
	Status: fiber.StatusForbidden, Code: "token_not_allowed",
	Message: "this can only be done from a browser session",
}

// errTooManyTokens is [apitoken.ErrTooMany] as a response.
var errTooManyTokens = &fiberx.APIError{
	Status: fiber.StatusConflict, Code: "too_many_tokens",
	Message: "you have the maximum number of API tokens",
}

// ResolveAPIToken recognises `Authorization: Bearer <prefix>…` and acts as the
// token's maker for the rest of the request. [New] mounts it when
// [Options.Tokens] is set — after the session middleware, so a token replaces a
// session rather than being overwritten by one, and after the gate's own routes,
// so whoami, the session list and the handshake answer for the cookie alone.
//
// Like the session middleware it does nothing while no login is configured: no
// header is read and the store is not touched. A bearer value without this
// application's prefix is left alone — it is somebody else's credential. It runs
// on every request, so the path without a token has to stay one header read.
//
// Unlike the session middleware it does refuse requests. One that presents a
// token which cannot be matched answers 401, whatever else it carries: falling
// through to a session or to anonymous would have a script with a revoked token
// carry on as somebody else, or against open routes as if nothing had happened.
// And a store that cannot be asked answers 503, because not knowing is not
// "anonymous".
//
// The token itself goes no further than this function: only its hash leaves it.
func (g *Gate) ResolveAPIToken() fiber.Handler {
	if g.tokens == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}
	return recognizer{
		provider: g.Provider,
		tokens:   g.tokens,
		prefix:   g.tokenPrefix,
		logger:   g.logger,
		// Through the gate, not a copy of g.now: the clock is swapped after New.
		now: func() time.Time { return g.now() },
	}.handle
}

// APITokenMiddleware is [Gate.ResolveAPIToken] for an application that manages
// tokens through routes of its own: it recognises a token exactly as a gate with
// [Options.Tokens] does, and mounts nothing. Such an application has reasons the
// gate's six routes cannot know — a path its clients already call, a sign-in
// flow that ends in a token — but none to recognise a token differently, and
// that half is the one a mistake in costs something.
//
// Leave [Options.Tokens] unset and mount this yourself, after [New] and ahead of
// your own routes — the same position the gate gives it:
//
//	gate := fiberauth.New(a, fiberauth.Options{Provider: provider, Sessions: sessions})
//	a.HTTP.Use(fiberauth.APITokenMiddleware(provider, tokens, "myapp_"))
//
// provider is the one the gate was given; nil, or returning nil, means no login
// and the middleware passes everything through. [ViaAPIToken], [APITokenID] and
// [RefuseAPIToken] work behind it as they do behind a gate.
//
// It panics on a nil store or an empty prefix. Both are wiring mistakes, and the
// second would claim every bearer value as this application's and answer 401 to
// credentials meant for something else.
func APITokenMiddleware(provider func() *oidcauth.Provider, tokens apitoken.Lookup, prefix string) fiber.Handler {
	if tokens == nil {
		panic("fiberauth: APITokenMiddleware needs a token store")
	}
	if prefix == "" {
		panic("fiberauth: APITokenMiddleware needs a token prefix")
	}
	return recognizer{
		provider: func() *oidcauth.Provider {
			if provider == nil {
				return nil
			}
			return provider()
		},
		tokens: tokens,
		prefix: prefix,
		logger: slog.Default,
		now:    time.Now,
	}.handle
}

// recognizer is what recognising a token needs, however it came to be mounted.
// Every field is set; the two constructors above are the only ones.
type recognizer struct {
	provider func() *oidcauth.Provider
	tokens   apitoken.Lookup
	prefix   string
	logger   func() *slog.Logger
	now      func() time.Time
}

func (r recognizer) handle(c fiber.Ctx) error {
	provider := r.provider()
	if provider == nil {
		return c.Next()
	}
	secret, ok := bearerToken(c.Get(fiber.HeaderAuthorization), r.prefix)
	if !ok {
		return c.Next()
	}

	ctx := c.Context()
	token, found, err := r.tokens.TokenByHash(ctx, apitoken.Hash(secret))
	if err != nil {
		// The store's error does not hold the hash: database/sql leaves
		// arguments out of what it reports.
		r.logger().ErrorContext(ctx, "API token lookup failed", logx.Err(err))
		return fiber.NewError(fiber.StatusServiceUnavailable, "could not check the API token")
	}
	now := r.now().UTC()
	// One answer for unknown, expired and made-under-another-login, so it does
	// not say which tokens once existed.
	if !found || token.Issuer != provider.Issuer() || !token.Live(now) {
		c.Set(fiber.HeaderWWWAuthenticate, "Bearer")
		return fiber.NewError(fiber.StatusUnauthorized, "invalid API token")
	}

	if token.NeedsTouch(now, apitoken.DefaultTouchInterval) {
		// Freshness is not worth failing a request the caller is entitled to.
		if err := r.tokens.TouchToken(ctx, token.ID, now); err != nil {
			r.logger().WarnContext(ctx, "API token touch failed", logx.Err(err))
		}
	}

	id := token.Identity()
	// Derived per request, as for a session: the owner group in force decides,
	// so changing it applies at once. The groups are the snapshot from when the
	// token was made.
	id.Owner = provider.IsOwner(id.Groups)
	c.Locals(localIdentityKey, &id)
	c.Locals(localTokenKey, token.ID)
	return c.Next()
}

// bearerToken pulls one of this application's own tokens out of an Authorization
// header. The scheme is case-insensitive by RFC 9110; the token is not.
func bearerToken(header, prefix string) (string, bool) {
	if header == "" {
		return "", false
	}
	scheme, value, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	return value, true
}

// ViaAPIToken reports whether this request was authenticated by a token rather
// than a browser session.
func ViaAPIToken(c fiber.Ctx) bool {
	_, ok := APITokenID(c)
	return ok
}

// APITokenID is the row id of the token this request was authenticated by, which
// is what lets a token revoke itself. Never the secret: that does not outlive
// [Gate.ResolveAPIToken].
func APITokenID(c fiber.Ctx) (string, bool) {
	id, ok := c.Locals(localTokenKey).(string)
	return id, ok && id != ""
}

// RefuseAPIToken guards a route for a browser session, answering a token with
// [ErrTokenNotAllowed].
//
// Put it on any route that could mint credentials, change who may sign in, or
// hand over a stored secret. Those take someone at a browser, who signed in
// within the session lifetime; a token is a long-lived secret in a file, and
// the difference is the whole point of having both.
//
// Mount it ahead of the owner guard, so an owner's token and a member's get the
// same answer.
func RefuseAPIToken(c fiber.Ctx) error {
	if ViaAPIToken(c) {
		return ErrTokenNotAllowed
	}
	return c.Next()
}

// MyTokensPath is the signed-in person's own tokens.
func (g *Gate) MyTokensPath() string { return g.apiBase + "/auth/me/tokens" }

// CurrentTokenPath is the token a request presents — DELETE it to sign an app
// out.
func (g *Gate) CurrentTokenPath() string { return g.apiBase + "/auth/me/token" }

// TokensPath is the owner-only list of everyone's tokens.
func (g *Gate) TokensPath() string { return g.apiBase + "/auth/tokens" }

// registerTokens mounts the token middleware and then the routes that manage
// tokens — after it, so they can tell a token from a session and refuse the
// former by name rather than as a bare 401.
func (g *Gate) registerTokens(app *fiber.App) {
	app.Use(g.ResolveAPIToken())

	// 404 with no login: there is no "you" to hold a token, and an open
	// deployment exposes no auth surface. Then the token refusal, then the
	// identity — so a token gets 403 token_not_allowed, not a 401 that suggests
	// its credential was wrong.
	configured, noToken := g.requireConfigured(), RefuseAPIToken
	signedIn, owner := g.RequireAuth(), g.RequireOwner()

	app.Get(g.MyTokensPath(), configured, noToken, signedIn, g.listMyTokens())
	app.Post(g.MyTokensPath(), configured, noToken, signedIn, g.createToken())
	app.Delete(g.MyTokensPath()+"/:id", configured, noToken, signedIn, g.deleteMyToken())

	// The one token route a token may call, because it can only ever shrink what
	// the token can do.
	app.Delete(g.CurrentTokenPath(), configured, g.deleteCurrentToken())

	app.Get(g.TokensPath(), configured, noToken, owner, g.listAllTokens())
	app.Delete(g.TokensPath()+"/:id", configured, noToken, owner, g.revokeToken())
}

// TokenView is a token as its lists show it. The hash and the identity snapshot
// have no place in it.
type TokenView struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	// ExpiresAt is null for a token that does not expire.
	ExpiresAt *string `json:"expiresAt"`
}

// CreatedTokenView is the one response that ever carries the token.
type CreatedTokenView struct {
	TokenView
	Token string `json:"token"`
}

// OwnedTokenView is a token in the owner's overview: whose it is, and whether
// it still works.
type OwnedTokenView struct {
	TokenView
	Subject string `json:"subject"`
	Person  string `json:"person"`
	Email   string `json:"email"`
	// Active is false for a token that has expired or was made under a login
	// that is no longer the one in force. Such a row grants nothing; it is listed
	// so it can be cleared.
	Active bool `json:"active"`
}

func viewOf(t apitoken.Token) TokenView {
	return TokenView{
		ID: t.ID, Name: t.Name,
		CreatedAt:  t.CreatedAt.UTC().Format(time.RFC3339),
		LastUsedAt: stampPtr(t.LastUsedAt),
		ExpiresAt:  stampPtr(t.ExpiresAt),
	}
}

func stampPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// caller is the person asking and the login they are signed in under. The
// guards ahead of every route that calls it have already refused the cases in
// which either is missing; this reads both once, so a login that changes halfway
// through a request cannot leave a row under an empty issuer.
func (g *Gate) caller(c fiber.Ctx) (issuer string, id *oidcauth.Identity, err error) {
	provider := g.Provider()
	id, ok := IdentityFrom(c)
	if provider == nil || !ok {
		return "", nil, fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	return provider.Issuer(), id, nil
}

func (g *Gate) storeFailed(c fiber.Ctx, what string, err error) error {
	g.logger().ErrorContext(c.Context(), what, logx.Err(err))
	return fiber.NewError(fiber.StatusInternalServerError, what)
}

func (g *Gate) listMyTokens() fiber.Handler {
	return func(c fiber.Ctx) error {
		issuer, id, err := g.caller(c)
		if err != nil {
			return err
		}
		tokens, err := g.tokens.ListTokens(c.Context(), issuer, id.Subject)
		if err != nil {
			return g.storeFailed(c, "could not list the API tokens", err)
		}
		views := make([]TokenView, 0, len(tokens))
		for _, t := range tokens {
			views = append(views, viewOf(t))
		}
		return c.JSON(views)
	}
}

// tokenName is a token's name, trimmed as it is read. Validation has to see the
// trimmed value: "1 to 64 characters" is about what ends up on a screen, and
// three spaces are not a name.
type tokenName string

func (n *tokenName) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	*n = tokenName(strings.TrimSpace(s))
	return nil
}

type createTokenRequest struct {
	Name tokenName `json:"name" validate:"required,max=64"` // apitoken.NameMaxLen
	// ExpiresInDays is optional. Without it the token lives as long as
	// Options.TokenMaxAge allows.
	ExpiresInDays *int `json:"expiresInDays" validate:"omitempty,min=1,max=3650"`
}

// createToken makes a token and shows it this once. Only its hash is stored.
func (g *Gate) createToken() fiber.Handler {
	return func(c fiber.Ctx) error {
		issuer, id, err := g.caller(c)
		if err != nil {
			return err
		}
		var req createTokenRequest
		if err := fiberx.BindAndValidate(c, g.validate, &req); err != nil {
			return err
		}

		now := g.now().UTC().Truncate(time.Second)
		secret, hash := apitoken.NewSecret(g.tokenPrefix)
		token := apitoken.Token{
			ID:          apitoken.NewID(),
			Name:        string(req.Name),
			Issuer:      issuer,
			Subject:     id.Subject,
			Email:       id.Email,
			DisplayName: id.Name,
			Groups:      id.Groups,
			CreatedAt:   now,
			ExpiresAt:   g.tokenExpiry(now, req.ExpiresInDays),
		}
		if err := g.tokens.CreateToken(c.Context(), token, hash); err != nil {
			if errors.Is(err, apitoken.ErrTooMany) {
				return errTooManyTokens
			}
			return g.storeFailed(c, "could not create the API token", err)
		}
		// The body holds a credential: no cache between here and the caller may
		// keep it.
		c.Set(fiber.HeaderCacheControl, "no-store")
		return fiberx.Created(c, fiberx.MemberURL(c, token.ID), CreatedTokenView{TokenView: viewOf(token), Token: secret})
	}
}

// tokenExpiry is when a token made at now stops working: what was asked for,
// never past what the deployment allows. Nil means it does not expire.
func (g *Gate) tokenExpiry(now time.Time, days *int) *time.Time {
	var expires time.Time
	if days != nil {
		expires = now.Add(time.Duration(*days) * 24 * time.Hour)
	}
	if g.tokenMaxAge > 0 {
		if latest := now.Add(g.tokenMaxAge); expires.IsZero() || expires.After(latest) {
			expires = latest
		}
	}
	if expires.IsZero() {
		return nil
	}
	return &expires
}

// deleteMyToken revokes one of the caller's tokens. It takes effect at once,
// since every request looks its token up. Someone else's answers 404, like one
// that does not exist.
func (g *Gate) deleteMyToken() fiber.Handler {
	return func(c fiber.Ctx) error {
		issuer, id, err := g.caller(c)
		if err != nil {
			return err
		}
		return g.deleted(c, g.tokens.DeleteToken(c.Context(), fiberx.PathParam(c, "id"), issuer, id.Subject))
	}
}

// deleteCurrentToken revokes the token this request presents: how an app signs
// out. A session has no token to revoke; its way out is logout.
func (g *Gate) deleteCurrentToken() fiber.Handler {
	return func(c fiber.Ctx) error {
		tokenID, ok := APITokenID(c)
		if !ok {
			return fiber.NewError(fiber.StatusBadRequest, "no API token on this request; a browser signs out with logout")
		}
		issuer, id, err := g.caller(c)
		if err != nil {
			return err
		}
		return g.deleted(c, g.tokens.DeleteToken(c.Context(), tokenID, issuer, id.Subject))
	}
}

func (g *Gate) listAllTokens() fiber.Handler {
	return func(c fiber.Ctx) error {
		tokens, err := g.tokens.ListAllTokens(c.Context())
		if err != nil {
			return g.storeFailed(c, "could not list the API tokens", err)
		}
		issuer := ""
		if p := g.Provider(); p != nil {
			issuer = p.Issuer()
		}
		now := g.now().UTC()
		views := make([]OwnedTokenView, 0, len(tokens))
		for _, t := range tokens {
			views = append(views, OwnedTokenView{
				TokenView: viewOf(t),
				Subject:   t.Subject, Person: t.DisplayName, Email: t.Email,
				// The same two checks the middleware applies, so the list never calls
				// a token active that a request would be refused with.
				Active: t.Issuer == issuer && t.Live(now),
			})
		}
		return c.JSON(views)
	}
}

func (g *Gate) revokeToken() fiber.Handler {
	return func(c fiber.Ctx) error {
		return g.deleted(c, g.tokens.RevokeToken(c.Context(), fiberx.PathParam(c, "id")))
	}
}

// deleted answers a removal: 204, or "no such token" as the store said it —
// errs.ErrNotFound renders as 404 with no help from here.
func (g *Gate) deleted(c fiber.Ctx, err error) error {
	switch {
	case err == nil:
		return c.SendStatus(fiber.StatusNoContent)
	case errors.Is(err, errs.ErrNotFound):
		return err
	default:
		return g.storeFailed(c, "could not revoke the API token", err)
	}
}
