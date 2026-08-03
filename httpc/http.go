package httpc

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// DefaultHTTPTimeout is the overall request budget applied when
// [HTTPOptions.Timeout] is zero. It matches [NewBaseClient]'s.
const DefaultHTTPTimeout = 15 * time.Second

// NoTimeout disables the client's overall timeout, for cases where the caller's
// context is the real bound — a long-poll, a streamed download.
//
// It is a named constant rather than a bare 0 because a zero [HTTPOptions.Timeout]
// means "give me the default": forgetting the field cannot then accidentally
// produce a client that hangs forever, and going unbounded stays greppable.
const NoTimeout = -1

// HTTPOptions configures [NewHTTPClient]. The zero value is the house default: a
// 15s budget, the standard retry policy, and no tracing.
type HTTPOptions struct {
	// Timeout is the overall budget for a request, including its retries. Zero
	// means DefaultHTTPTimeout; NoTimeout means none.
	Timeout time.Duration
	// Transport is the base RoundTripper to wrap — use it for a custom TLS config
	// or a self-signed LAN device. Nil means http.DefaultTransport. It is wrapped,
	// never replaced: the retry layer always sits on top.
	Transport http.RoundTripper
	// Retry overrides the backoff policy. Nil means DefaultRetryConfig(). A
	// RetryConfig with MaxRetries: 0 issues each request exactly once.
	Retry *RetryConfig
	// Tracing opens an otel client span per request attempt and carries the
	// traceparent header outbound. Set it where a collector actually runs: the
	// wrapper costs roughly +1.2µs and +22 allocations per request even with no
	// provider installed, which is why it is opt-in.
	Tracing bool
}

// NewHTTPClient builds an *http.Client carrying [RetryTransport]. It is the
// sanctioned way to build a client outside [BaseClient] — a hand-rolled
// &http.Client{} silently loses retries and, with Tracing set, spans.
//
//	httpc.NewHTTPClient(httpc.HTTPOptions{})                       // house default
//	httpc.NewHTTPClient(httpc.HTTPOptions{Timeout: 3 * time.Minute})
//	httpc.NewHTTPClient(httpc.HTTPOptions{Timeout: httpc.NoTimeout}) // ctx bounds it
//	httpc.NewHTTPClient(httpc.HTTPOptions{Tracing: true})            // + otel spans
//
// For a WebSocket handshake use [NewWebSocketHTTPClient] instead.
func NewHTTPClient(opts HTTPOptions) *http.Client {
	timeout := opts.Timeout
	switch {
	case timeout == 0:
		timeout = DefaultHTTPTimeout
	case timeout < 0: // NoTimeout (or any negative): let the context bound it
		timeout = 0
	}

	cfg := DefaultRetryConfig()
	if opts.Retry != nil {
		cfg = *opts.Retry
	}

	rt := NewRetryTransport(opts.Transport, cfg)
	if opts.Tracing {
		rt = NewTracedRetryTransport(opts.Transport, cfg)
	}
	return &http.Client{Timeout: timeout, Transport: rt}
}

// NewWebSocketHTTPClient returns a client for a WebSocket opening handshake.
//
// It installs no retry or otel transport and no timeout, and both omissions are
// load-bearing: those wrappers wrap the response body, but a WS upgrade needs the
// raw hijacked connection, so a wrapped body fails the dial at runtime; and an
// http.Client.Timeout would tear down the long-lived socket mid-session.
//
// Use it only for the handshake. Fetch anything else the device serves with a
// normal [NewHTTPClient], so those requests keep their retries.
func NewWebSocketHTTPClient(tlsConfig *tls.Config) *http.Client {
	return &http.Client{Transport: cloneTransport(tlsConfig)}
}

// InsecureTLSTransport returns a transport with certificate verification
// disabled, for the self-signed LAN devices a host integrates.
func InsecureTLSTransport() *http.Transport {
	return cloneTransport(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // self-signed LAN device by design
}

// cloneTransport returns http.DefaultTransport with tlsConfig applied. Cloning
// rather than building fresh preserves proxy support, HTTP/2, pooling and the
// handshake/idle timeouts; it falls back to a bare transport only if
// DefaultTransport was replaced, where the type assertion would otherwise panic.
func cloneTransport(tlsConfig *tls.Config) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{TLSClientConfig: tlsConfig}
	}
	t := base.Clone()
	t.TLSClientConfig = tlsConfig
	return t
}

// CheckStatus returns a *[APIError] when resp carries a non-2xx status, or nil
// when it is 2xx — leaving the body unread and intact for the caller to decode.
//
// It gives a hand-rolled client the same structured error [BaseClient.DoJSON]
// produces, so errors.As(&APIError) and [IsConflict] keep working. A client that
// formats the status into a string instead ("myservice: HTTP 409") makes it
// impossible for any caller to branch on it.
func CheckStatus(service string, resp *http.Response) error {
	if resp == nil {
		return fmt.Errorf("%s: no response", service)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := ReadAllLimited(resp.Body, errorBodyLimit)
	return newAPIError(service, resp.StatusCode, raw)
}

// StatusError returns an *[APIError] for a status the caller already holds,
// without a live *http.Response — for helpers that return a bare (status, body)
// pair so the caller can branch on the status itself.
//
// The body is deliberately omitted: an upstream body can carry credentials, and
// the caller has it anyway. errors.As and [IsConflict] work as with [CheckStatus].
func StatusError(service string, status int) error {
	return newAPIError(service, status, nil)
}

// Redact returns err with credential-bearing detail stripped. For a *url.Error —
// what every net/http transport failure is — it drops the query string and any
// userinfo, keeping scheme://host/path.
//
// Wrap any transport error from a client whose auth rides in the URL rather than
// a header (an api_key query parameter); otherwise the token travels inside
// err.Error() into logs and out through any API that surfaces them. Apply it to
// the raw transport error, before adding context with %w. It is a no-op for
// errors carrying no URL, so it is always safe to apply.
func Redact(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	safe := *urlErr
	safe.URL = RedactURL(urlErr.URL)
	return &safe
}

// RedactURL strips the credential-bearing parts of a URL — query string,
// fragment and userinfo — keeping scheme://host/path.
//
// Apply it anywhere a request URL is stored or surfaced rather than dialled: a
// [DebugRing] served from a diagnostics endpoint leaks an api_key query
// parameter exactly as readily as a log line does.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// Unparseable: drop it entirely rather than risk emitting a token.
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String()
}
