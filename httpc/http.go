package httpc

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// This file owns how a plugin gets an *http.Client. There are exactly two
// sanctioned ways — NewHTTPClient for everything that speaks HTTP, and
// NewWebSocketHTTPClient for a WebSocket opening handshake — and a bare
// &http.Client{} is rejected by the conventions test in internal/plugin.
//
// The reason is invisible until it bites: RetryTransport is also the seam that
// installs otelhttp (see transport.go), so a hand-rolled client silently loses
// distributed tracing, not just retries. Nothing in the type system says so,
// which is exactly why it is a constructor and a lint rather than a doc line.

// DefaultHTTPTimeout is the overall request budget applied when
// [HTTPOptions.Timeout] is zero. It matches [NewBaseClient]'s, so the two
// sanctioned paths agree.
//
// A plugin overrides it only with a reason worth writing down: an LLM completion
// (ollama) or an interactive auth flow (fluvius) legitimately outruns it. "The
// upstream felt slow once" does not.
const DefaultHTTPTimeout = 15 * time.Second

// NoTimeout disables the client's overall timeout. Use it only where the caller's
// context is the real bound — a long-poll, a streamed download. It is a named
// constant rather than a bare 0 so that an unbounded client is always a
// deliberate, greppable choice: a zero Timeout in [HTTPOptions] means "give me
// the default", so forgetting the field cannot accidentally produce a client
// that hangs forever.
const NoTimeout = -1

// HTTPOptions configures [NewHTTPClient]. The zero value is the house default: a
// 15s budget, the standard retry policy, and otel client spans.
type HTTPOptions struct {
	// Timeout is the overall budget for a request, including its retries. Zero
	// means DefaultHTTPTimeout; NoTimeout means none.
	Timeout time.Duration
	// Transport is the base RoundTripper to wrap — use it for a custom TLS config
	// or a self-signed LAN device. Nil means http.DefaultTransport. It is wrapped,
	// never replaced: the retry and tracing layers always sit on top.
	Transport http.RoundTripper
	// Retry overrides the backoff policy. Nil means DefaultRetryConfig(). A
	// RetryConfig with MaxRetries: 0 issues each request exactly once while
	// keeping the otel span.
	Retry *RetryConfig
}

// NewHTTPClient builds an *http.Client that always carries the host's
// [RetryTransport] — and therefore its otel client spans. It is the only
// sanctioned way to build a client outside [BaseClient]:
//
//	http: neogate.NewHTTPClient(neogate.HTTPOptions{})                       // house default
//	http: neogate.NewHTTPClient(neogate.HTTPOptions{Timeout: 3 * time.Minute}) // documented exception
//	http: neogate.NewHTTPClient(neogate.HTTPOptions{Timeout: neogate.NoTimeout}) // long-poll; ctx bounds it
//
// For a WebSocket handshake use [NewWebSocketHTTPClient] instead — the retry and
// tracing wrappers wrap the response body, which a WS upgrade cannot survive.
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

	return &http.Client{
		Timeout:   timeout,
		Transport: NewRetryTransportConfig(opts.Transport, cfg),
	}
}

// NewWebSocketHTTPClient returns a client for a WebSocket opening handshake, and
// is the one sanctioned exception to "every client carries RetryTransport".
//
// It deliberately installs NO retry/otel transport: those wrap the response body,
// and a WS upgrade needs the raw hijacked connection — a wrapped body makes the
// dial fail at runtime, which no unit test would catch. It also sets no timeout:
// the dial context is the bound, and an http.Client.Timeout would tear down the
// long-lived socket mid-session.
//
// Use it ONLY for the handshake. Fetch anything else the device serves (icons,
// thumbnails, its REST API) with a normal [NewHTTPClient], so those requests keep
// their retries and spans.
//
//	c.ws = neogate.NewWebSocketHTTPClient(&tls.Config{InsecureSkipVerify: true}) // self-signed LAN device
//	c.api = neogate.NewHTTPClient(neogate.HTTPOptions{})
func NewWebSocketHTTPClient(tlsConfig *tls.Config) *http.Client {
	transport := http.DefaultTransport
	if tlsConfig != nil {
		// Clone the default transport so a custom TLS config keeps its proxy support,
		// HTTP/2 and pooling; fall back to a bare transport only if DefaultTransport
		// was replaced (the assertion would otherwise panic).
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			t := base.Clone()
			t.TLSClientConfig = tlsConfig
			transport = t
		} else {
			transport = &http.Transport{TLSClientConfig: tlsConfig}
		}
	}
	// No Timeout, and no RetryTransport: see the doc comment. Both are load-bearing.
	return &http.Client{Transport: transport}
}

// InsecureTLSTransport returns a clone of http.DefaultTransport with certificate
// verification disabled — for the self-signed LAN devices NeoGate integrates
// (UniFi consoles, Proxmox nodes, webOS TVs). Cloning preserves proxy support,
// HTTP/2, connection pooling and the handshake/idle timeouts; only verification
// changes. Falls back to a bare transport only if DefaultTransport was replaced.
func InsecureTLSTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // self-signed LAN device by design
	}
	t := base.Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // self-signed LAN device by design
	return t
}

// CheckStatus returns a *[APIError] when resp carries a non-2xx status, or nil
// when it is 2xx — leaving the body unread and intact for the caller to decode.
//
// It gives a hand-rolled client the same structured error [BaseClient.DoJSON]
// produces, so errors.As(&APIError) and [IsConflict] work against every plugin
// rather than only the ones that embed BaseClient. Without it, a client that
// formats the status into a string ("myservice: HTTP 409") makes it impossible
// for any caller to branch on the status.
//
//	resp, err := c.http.Do(req)
//	if err != nil { return fmt.Errorf("myservice: %w", Redact(err)) }
//	defer resp.Body.Close()
//	if err := neogate.CheckStatus(ServiceID, resp); err != nil { return err }
//
// The error body is bounded (8 KiB) for the same reason BaseClient bounds it: a
// misbehaving upstream must not stream megabytes into an error string.
func CheckStatus(service string, resp *http.Response) error {
	if resp == nil {
		return fmt.Errorf("%s: no response", service)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := ReadAllLimited(resp.Body, 8<<10)
	return &APIError{Service: service, StatusCode: resp.StatusCode, Body: string(raw)}
}

// StatusError returns an *[APIError] for a status the caller already holds,
// without a live *http.Response.
//
// It exists for clients whose transport helper returns a bare (status, body) pair
// so the caller can branch on the status itself — a 403 that means "re-login", a
// 408 that means "the car is asleep", a 204 that means "nothing playing". Those
// cannot use [CheckStatus] (the response is already consumed), and hand-rolling
// an &APIError{…} in the plugin is precisely the drift the SDK exists to prevent.
//
//	status, body, err := c.get(ctx, path)
//	if err != nil { return err }
//	if status == http.StatusForbidden { return c.reauth(ctx) }   // caller's own branch
//	if status < 200 || status >= 300 {
//	    return fmt.Errorf("%s fetch: %w", ServiceID, neogate.StatusError(ServiceID, status))
//	}
//
// The body is deliberately omitted: an upstream body can carry credentials, and by
// this point the caller has it anyway. errors.As and [IsConflict] work on the
// result exactly as they do on CheckStatus's.
func StatusError(service string, status int) error {
	return &APIError{Service: service, StatusCode: status}
}

// Redact returns err with credential-bearing detail stripped. For a *url.Error —
// what every net/http transport failure is — it drops the query string and any
// userinfo, keeping scheme://host/path.
//
// Wrap any transport error from a client whose auth rides in the URL rather than
// a header (an api_key or securityToken query parameter). Otherwise the token
// travels inside err.Error() into logs, into the activity feed, and out through
// the management API.
//
//	resp, err := c.http.Do(req)
//	if err != nil {
//	    return fmt.Errorf("%s request: %w", ServiceID, neogate.Redact(err))
//	}
//
// It is a no-op for errors that carry no URL, so it is always safe to apply.
// Apply it to the raw transport error, before adding your own context with %w.
func Redact(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	safe := *urlErr
	if u, perr := url.Parse(urlErr.URL); perr == nil {
		u.RawQuery = ""
		u.Fragment = ""
		u.User = nil
		safe.URL = u.String()
	} else {
		// Unparseable: drop it entirely rather than risk emitting a token.
		safe.URL = ""
	}
	return &safe
}
