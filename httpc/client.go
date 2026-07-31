// Package httpc is the HTTP client core: a BaseClient for JSON APIs with
// built-in retries, bearer-token injection/refresh, and bounded reads; a
// Fault classification (see fault.go) that turns an error into "what should
// the caller do about it" instead of a status code to grep; and an
// SSRF-guarded transport (see ssrf.go) for fetching URLs a caller only
// partly controls. NewHTTPClient and NewWebSocketHTTPClient (see http.go)
// are the two sanctioned ways to obtain an *http.Client — both carry the
// retry/tracing transport a hand-rolled client would silently lose.
package httpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// APIError is returned when a server responds with a non-2xx status. Callers use
// errors.As to branch on StatusCode (e.g. to detect 401 for token refresh)
// instead of matching on the error string.
type APIError struct {
	Service    string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	// A body-less APIError (see StatusError) must not render a dangling "403: ".
	if e.Body == "" {
		return fmt.Sprintf("%s API %d", e.Service, e.StatusCode)
	}
	return fmt.Sprintf("%s API %d: %s", e.Service, e.StatusCode, e.Body)
}

// errorBodyLimit bounds how much of a non-2xx body is kept in an [APIError]: a
// misbehaving upstream must not stream megabytes into an error string.
const errorBodyLimit = 8 << 10

// newAPIError is the single construction point for a status error, so
// [CheckStatus], [StatusError] and [BaseClient.send] cannot drift on how one is
// shaped. A nil body means "status only".
func newAPIError(service string, status int, body []byte) *APIError {
	return &APIError{Service: service, StatusCode: status, Body: string(body)}
}

// AuthFunc sets authentication headers on an outgoing request.
type AuthFunc func(req *http.Request)

// HeaderAuth returns an AuthFunc that sets a single header (e.g. X-Api-Key).
func HeaderAuth(key, value string) AuthFunc {
	return func(req *http.Request) {
		req.Header.Set(key, value)
	}
}

// BearerAuth returns an AuthFunc that sets a Bearer token.
func BearerAuth(token string) AuthFunc {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// BasicAuth returns an AuthFunc that sets HTTP Basic credentials.
func BasicAuth(username, password string) AuthFunc {
	return func(req *http.Request) {
		req.SetBasicAuth(username, password)
	}
}

// TokenSource supplies a bearer token for a BaseClient with a configured one.
// Token is called before each request with stale == "": use the cached token,
// refreshing only if it's missing or expired. On the single retry after a 401 the
// caller passes the token it just used as stale; the source re-logs in only if
// that token is still the cached one. If a concurrent caller already refreshed,
// the newer cached token is returned without another login — so a burst of
// simultaneous 401 retries (several in-flight requests sharing one client)
// collapses into a single re-login instead of stampeding a rate-limited auth
// endpoint. Implementations own their caching and concurrency. Use this (instead
// of a static [BearerAuth]) when auth is stateful — lazy login, token expiry,
// re-auth on 401.
type TokenSource interface {
	Token(ctx context.Context, stale string) (string, error)
}

// LoginFunc authenticates and returns a fresh token plus its lifetime. ttl is
// the token's full validity; when it exceeds the refresh margin the caching
// source renews early by that margin so a request can't race expiry. A ttl at or
// below the margin is kept until real expiry (the one-shot 401 retry is the
// backstop), and a non-positive ttl means "unknown lifetime" — reused until a
// 401 forces a refresh.
type LoginFunc func(ctx context.Context) (token string, ttl time.Duration, err error)

// tokenRefreshMargin is how far before real expiry a cached token is renewed.
const tokenRefreshMargin = time.Minute

// NewCachingTokenSource adapts a [LoginFunc] into a [TokenSource] that caches
// the token and re-runs login only when it is missing, near expiry, or after a
// forced refresh (the post-401 retry). It serializes callers so login runs at
// most once per refresh, and owns all token state — a plugin supplies just its
// auth call. Use it instead of hand-rolling the mutex + expiry bookkeeping.
//
//	bc.Tokens = httpc.NewCachingTokenSource(func(ctx context.Context) (string, time.Duration, error) {
//	    return login(ctx) // returns (token, ttl, err)
//	})
func NewCachingTokenSource(login LoginFunc) TokenSource {
	return &cachingTokenSource{login: login, margin: tokenRefreshMargin}
}

type cachingTokenSource struct {
	login  LoginFunc
	margin time.Duration

	mu        sync.Mutex
	token     string
	expiresAt time.Time // zero means "no known expiry"; refresh only on force/401
}

func (c *cachingTokenSource) Token(ctx context.Context, stale string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	forced := stale != ""
	if !forced && c.token != "" && !c.expired() {
		return c.token, nil
	}
	// Stampede guard: a burst of concurrent 401 retries all pass the same stale
	// token. The first re-logs in and replaces c.token; the rest then see a cached
	// token that no longer matches their stale one and reuse it, instead of each
	// firing another (often rate-limited) login for a token that's already fresh.
	if forced && c.token != "" && c.token != stale {
		return c.token, nil
	}
	token, ttl, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.token = token
	c.setExpiry(ttl)
	return c.token, nil
}

func (c *cachingTokenSource) expired() bool {
	return !c.expiresAt.IsZero() && !time.Now().Before(c.expiresAt)
}

func (c *cachingTokenSource) setExpiry(ttl time.Duration) {
	if ttl <= 0 {
		c.expiresAt = time.Time{} // unknown lifetime; reuse until a 401
		return
	}
	if ttl > c.margin {
		ttl -= c.margin
	}
	c.expiresAt = time.Now().Add(ttl)
}

// BaseClient is an embeddable HTTP client that handles JSON request/response,
// error wrapping, and auth header injection. Integration clients embed this
// to avoid duplicating boilerplate.
//
//	type client struct { httpc.BaseClient }
//
//	func newClient(url, apiKey string) *client {
//	    return &client{BaseClient: httpc.NewBaseClient(url, "myservice", httpc.HeaderAuth("X-Api-Key", apiKey))}
//	}
type BaseClient struct {
	BaseURL    string
	HTTPClient *http.Client
	Auth       AuthFunc
	Tokens     TokenSource // optional; when set, injects a bearer token + retries once on 401
	Service    string      // prefix for error messages
	Debug      *DebugRing  // optional; when set, captures outbound request summaries
}

// NewBaseClient creates a BaseClient with sensible defaults: a
// [DefaultHTTPTimeout] overall budget and a RetryTransport that retries
// idempotent reads on transient failures (and, via the same seam, opens an otel
// client span per attempt). Because the retry policy only replays idempotent
// methods, the provisioning writes that ride on DoJSON (POST/PUT/DELETE) are
// still issued exactly once.
//
// To change the budget, replace the client through the same sanctioned
// constructor rather than hand-rolling one, so the retry/otel transport survives:
//
//	bc := httpc.NewBaseClient(url, ServiceID, auth)
//	bc.HTTPClient = httpc.NewHTTPClient(httpc.HTTPOptions{Timeout: 3 * time.Minute})
func NewBaseClient(baseURL, service string, auth AuthFunc) BaseClient {
	return BaseClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: NewHTTPClient(HTTPOptions{}),
		Service:    service,
		Auth:       auth,
	}
}

// DoJSON performs an HTTP request with optional JSON body and response decoding.
// It handles marshalling, auth injection, status checks, and error wrapping.
//
// When a [TokenSource] is configured (see [BaseClient.Tokens]), DoJSON injects a
// bearer token and, on a 401, refreshes the token and retries the request
// exactly once.
func (c *BaseClient) DoJSON(ctx context.Context, method, url string, body any, out any) error {
	var rawBody []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s marshal request: %w", c.Service, err)
		}
		rawBody = data
	}

	used, err := c.attempt(ctx, method, url, rawBody, out, "")
	if c.Tokens != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
			// Retry once, telling the token source which token was rejected so it
			// refreshes exactly once even when several requests 401 concurrently.
			_, err = c.attempt(ctx, method, url, rawBody, out, used)
		}
	}
	return err
}

// DoWithTokenRetry runs do with a valid bearer token from ts and, on an HTTP 401,
// retries once with a forced refresh — passing the rejected token as the stale
// marker so a burst of concurrent 401s refreshes the token only once. It is the
// token half of DoJSON's retry, factored out for clients that read raw (non-JSON)
// responses and drive their own request/decode. do receives the bearer token and
// returns the raw HTTP status, body, and any transport error; a non-401 status or
// a transport error is returned unchanged.
func DoWithTokenRetry(ctx context.Context, ts TokenSource, do func(token string) (status int, body []byte, err error)) (int, []byte, error) {
	token, err := ts.Token(ctx, "")
	if err != nil {
		return 0, nil, err
	}
	status, body, err := do(token)
	if err != nil || status != http.StatusUnauthorized {
		return status, body, err
	}
	fresh, err := ts.Token(ctx, token) // stale token → forced, deduped refresh
	if err != nil {
		return 0, nil, err
	}
	return do(fresh)
}

// attempt issues a single request and returns the bearer token it used (empty
// when no TokenSource is configured), so DoJSON can hand it back as the stale
// token on the post-401 retry. stale is forwarded to the TokenSource: "" on the
// first attempt, the rejected token on the retry.
func (c *BaseClient) attempt(ctx context.Context, method, url string, rawBody []byte, out any, stale string) (string, error) {
	var bodyReader io.Reader
	if rawBody != nil {
		bodyReader = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return "", fmt.Errorf("%s build request: %w", c.Service, err)
	}
	if c.Auth != nil {
		c.Auth(req)
	}
	var used string
	if c.Tokens != nil {
		token, err := c.Tokens.Token(ctx, stale)
		if err != nil {
			return "", fmt.Errorf("%s auth: %w", c.Service, err)
		}
		used = token
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	if rawBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.send(req, method, url)
	if err != nil {
		return used, err
	}
	defer drainClose(resp.Body)
	if out != nil {
		// Bounded like every other read here: a compromised or runaway upstream
		// must not stream unbounded bytes into memory.
		if err := json.NewDecoder(io.LimitReader(resp.Body, MaxResponseBytes)).Decode(out); err != nil {
			return used, fmt.Errorf("%s response decode failed: %w", c.Service, err)
		}
	}
	return used, nil
}

// send issues req, records one debug entry, and converts a non-2xx status into a
// bounded *APIError. On success the caller owns the still-open body and must
// close it; on any error the body is already drained and closed here (the caller
// may still read resp.StatusCode — e.g. to detect a 401 for a token refresh). It
// is the shared do/debug/status core behind both DoJSON's attempt and
// [BaseClient.Bytes].
func (c *BaseClient) send(req *http.Request, method, url string) (*http.Response, error) {
	start := time.Now()
	resp, err := c.HTTPClient.Do(req)
	duration := time.Since(start)
	if err != nil {
		if c.Debug != nil {
			c.Debug.Push(DebugEntry{
				Time:       start,
				Method:     method,
				URL:        url,
				DurationMs: duration.Milliseconds(),
				Error:      err.Error(),
			})
		}
		return nil, fmt.Errorf("%s API request failed: %w", c.Service, err)
	}
	if c.Debug != nil {
		c.Debug.Push(DebugEntry{
			Time:       start,
			Method:     method,
			URL:        url,
			StatusCode: resp.StatusCode,
			DurationMs: duration.Milliseconds(),
		})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		// errorBodyLimit bounds what is *stored*; drainClose keeps reading so the
		// connection stays reusable, which matters most when an upstream is unwell.
		drainClose(resp.Body)
		return resp, newAPIError(c.Service, resp.StatusCode, raw)
	}
	return resp, nil
}

// URL builds a full URL from the base and a path suffix with optional formatting.
func (c *BaseClient) URL(path string, args ...any) string {
	if len(args) > 0 {
		path = fmt.Sprintf(path, args...)
	}
	return c.BaseURL + path
}

// MaxResponseBytes is the default ceiling ReadAllLimited applies — generous for
// JSON/XML API payloads while bounding a misbehaving or hostile upstream.
const MaxResponseBytes = 8 << 20 // 8 MiB

// ReadAllLimited reads from r up to max bytes (MaxResponseBytes when max <= 0),
// returning an error if the source has more — so a truncated read is never
// silently treated as complete. Use it in place of io.ReadAll on a response body
// whose size the plugin doesn't control, so a compromised or runaway upstream
// can't stream unbounded bytes into memory. Pass a larger max for binary payloads
// like poster art.
func ReadAllLimited(r io.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = MaxResponseBytes
	}
	// Read one extra byte so "exactly max" is distinguishable from "over max".
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("response exceeds %d-byte limit", max)
	}
	return data, nil
}
