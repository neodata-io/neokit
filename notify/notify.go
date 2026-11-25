// Package notify sends short operational notifications to the places a
// self-hosted deployment already has: a generic JSON webhook, an ntfy topic, or
// an Apprise API server.
//
// [Notification] is the transport-agnostic shape all three renderers accept, so
// an application maps its own event to it once, rather than building a payload
// from its own domain types that no second kind of event can reuse. [Sender] is
// the interface to hold.
//
// # What a send does and does not do
//
//   - One attempt. Every backend takes a POST, which is not idempotent, so a
//     transient failure is reported instead of retried into a duplicate.
//   - No address guard. Targets are operator-configured and often on the LAN
//     (an Apprise or ntfy container), which a guard would block. Do not pass a
//     URL an end user chose; if you must, set [Options.HTTPClient] to one built
//     on httpc.GuardedTransport.
//   - No redirects. A 3xx is returned as an error: following it would turn the
//     POST into an empty GET, or replay the secret and body to another host.
//   - Errors name the target by host only, since the path is often the
//     credential (an Apprise key, an ntfy topic).
//
// The webhook secret is a static header, not a signature: use https.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/neodata-io/neokit/httpc"
)

// Level is the severity of a notification, and the one piece of routing every
// backend understands — as a colour, an icon, or a priority. It is a closed set:
// a backend must be able to map every value, which an open string cannot promise.
type Level string

const (
	LevelInfo    Level = "info"
	LevelSuccess Level = "success"
	LevelWarning Level = "warning"
	LevelFailure Level = "failure"
)

// Valid reports whether l is one of the defined levels. An unrecognised level is
// treated as [LevelInfo] by every renderer rather than rejected: a notification
// is best-effort, and dropping one over a typo'd severity is the wrong trade.
func (l Level) Valid() bool {
	switch l {
	case LevelInfo, LevelSuccess, LevelWarning, LevelFailure:
		return true
	}
	return false
}

// Notification is one message. Title and Body are required by convention —
// every backend renders both — and everything else is advisory: a backend that
// has no concept of a click-through URL or of tags simply ignores them.
type Notification struct {
	Title string
	Body  string
	Level Level

	// URL is where a click should lead, when the backend supports one.
	URL string

	// Tags are backend-specific hints (ntfy renders them as emoji). Optional.
	Tags []string

	// Data rides along in the JSON payload of the backends that carry one (the
	// webhook sender), for a consumer that wants the structured event rather than
	// the rendered sentence. Ignored by the text-oriented backends.
	Data map[string]any
}

// Sender delivers a notification. Implementations are safe for concurrent use.
//
// An error means this delivery failed, nothing more: notifications are
// best-effort by nature, so a caller should log and carry on rather than fail
// the operation that triggered it.
type Sender interface {
	// Name identifies the backend in logs and in configuration.
	Name() string
	// Send delivers n, honouring ctx.
	Send(ctx context.Context, n Notification) error
}

// DefaultTimeout bounds one delivery. A notification that has not landed in ten
// seconds has lost its value.
const DefaultTimeout = 10 * time.Second

// Options are the settings every sender shares. The zero value is valid.
type Options struct {
	// HTTPClient overrides the client. Nil builds one through httpc with
	// [DefaultTimeout], which is what you want unless you need a custom
	// transport (a self-signed internal endpoint) or tracing. It is copied, and
	// the copy never follows redirects whatever its CheckRedirect says.
	HTTPClient *http.Client

	// Timeout overrides [DefaultTimeout]. Ignored when HTTPClient is set.
	Timeout time.Duration
}

func (o Options) client() *http.Client {
	var c http.Client
	if o.HTTPClient != nil {
		c = *o.HTTPClient
	} else {
		timeout := o.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		c = *httpc.NewHTTPClient(httpc.HTTPOptions{Timeout: timeout})
	}
	// A followed redirect delivers nothing (POST becomes GET) or leaks the secret
	// and body to another host, so the 3xx is handed back as the response.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// drainLimit bounds how much of a response is read to keep the connection
// reusable; a backend that keeps writing is abandoned rather than waited on.
const drainLimit = 64 << 10

// post sends body to url and consumes the response, which is the identical tail
// of all three senders.
func post(ctx context.Context, client *http.Client, service, url string, body io.Reader, header http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("%s: build request for %s: %w", service, targetHost(url), withoutURL(err))
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		// A transport failure renders the whole URL, and the credential is as
		// often in the path as in the query string, so only the host survives.
		return fmt.Errorf("%s: send to %s: %w", service, targetHost(url), withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()

	// CheckStatus rather than a hand-rolled status test, so the result classifies
	// through httpc.Classify like every other upstream error: a 401 from a
	// notification backend is the admin's problem, a 503 is nobody's.
	err = httpc.CheckStatus(service, resp)
	// Drain after CheckStatus has taken the rejection reason, so the connection
	// returns to the pool instead of costing a handshake per notification.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	return err
}

// targetHost is the only part of a notification URL fit for an error message.
func targetHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "(unparseable URL)"
}

// withoutURL drops the URL a *url.Error carries and keeps its cause.
func withoutURL(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}
	return err
}

func (n Notification) level() Level {
	if n.Level.Valid() {
		return n.Level
	}
	return LevelInfo
}

// ── Webhook ─────────────────────────────────────────────────────────────────

// WebhookPayload is the JSON body a [Webhook] posts. It is exported because it
// is a wire contract: a consumer on the other end unmarshals exactly this.
type WebhookPayload struct {
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Level     Level          `json:"level"`
	URL       string         `json:"url,omitempty"`
	Tags      []string       `json:"tags,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

// Webhook posts a JSON payload to a URL.
type Webhook struct {
	url    string
	secret string
	client *http.Client
	now    func() time.Time
}

// NewWebhook builds a webhook sender. secret, when non-empty, is sent as
// X-Webhook-Secret so the receiver can authenticate the caller.
func NewWebhook(url, secret string, opts Options) *Webhook {
	return &Webhook{url: url, secret: secret, client: opts.client(), now: time.Now}
}

func (w *Webhook) Name() string { return "webhook" }

func (w *Webhook) Send(ctx context.Context, n Notification) error {
	body, err := json.Marshal(WebhookPayload{
		Title: n.Title, Body: n.Body, Level: n.level(),
		URL: n.URL, Tags: n.Tags, Data: n.Data,
		Timestamp: w.now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("webhook: marshal: %w", err)
	}
	h := http.Header{"Content-Type": {"application/json"}}
	if w.secret != "" {
		h.Set("X-Webhook-Secret", w.secret)
	}
	return post(ctx, w.client, w.Name(), w.url, bytes.NewReader(body), h)
}

// ── ntfy ────────────────────────────────────────────────────────────────────

// Ntfy posts to an ntfy topic (ntfy.sh or self-hosted).
type Ntfy struct {
	topicURL string
	token    string
	client   *http.Client
}

// NewNtfy builds an ntfy sender for a full topic URL, e.g.
// "https://ntfy.sh/my-topic". token, when non-empty, is sent as a bearer token
// for a protected topic.
func NewNtfy(topicURL, token string, opts Options) *Ntfy {
	return &Ntfy{topicURL: topicURL, token: token, client: opts.client()}
}

func (t *Ntfy) Name() string { return "ntfy" }

func (t *Ntfy) Send(ctx context.Context, n Notification) error {
	h := http.Header{
		// ntfy takes the title and the metadata as headers, with the body as the
		// raw message. Headers are latin-1 on the wire, so the title is sanitised:
		// a newline would let a title forge additional ntfy directives, and ntfy
		// itself rejects a malformed header outright.
		"Title":    {headerSafe(n.Title)},
		"Priority": {ntfyPriority(n.level())},
	}
	if tags := ntfyTags(n); tags != "" {
		h.Set("Tags", tags)
	}
	if n.URL != "" {
		h.Set("Click", headerSafe(n.URL))
	}
	if t.token != "" {
		h.Set("Authorization", "Bearer "+t.token)
	}
	return post(ctx, t.client, t.Name(), t.topicURL, strings.NewReader(n.Body), h)
}

// ntfyPriority maps a level onto ntfy's 1–5 scale. Warning and failure are
// raised above default so they reach a phone in do-not-disturb; success is left
// at default rather than lowered, since a completed job is what an operator most
// often waits for.
func ntfyPriority(l Level) string {
	switch l {
	case LevelFailure:
		return "5"
	case LevelWarning:
		return "4"
	default:
		return "3"
	}
}

// ntfyTags renders the caller's tags, falling back to one that encodes the level
// as an emoji when the caller supplied none.
func ntfyTags(n Notification) string {
	if len(n.Tags) > 0 {
		safe := make([]string, 0, len(n.Tags))
		for _, t := range n.Tags {
			// Commas separate tags on the wire, so a tag containing one would split
			// into two; drop the separator rather than the tag.
			if t = headerSafe(strings.ReplaceAll(t, ",", " ")); t != "" {
				safe = append(safe, t)
			}
		}
		return strings.Join(safe, ",")
	}
	switch n.level() {
	case LevelSuccess:
		return "white_check_mark"
	case LevelWarning:
		return "warning"
	case LevelFailure:
		return "rotating_light"
	default:
		return "bell"
	}
}

// headerSafe makes a caller-supplied string safe to put in an HTTP header:
// control characters are stripped (a CR or LF would let the value inject
// additional headers) and the result is length-bounded.
func headerSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 256 {
		out = out[:256]
	}
	return out
}

// ── Apprise ─────────────────────────────────────────────────────────────────

// Apprise posts to an Apprise API server (github.com/caronc/apprise-api),
// which fans one call out to any of Apprise's ~90 backends.
type Apprise struct {
	url    string
	client *http.Client
}

// NewApprise builds an Apprise sender. url points at the API's notify endpoint —
// "http://apprise:8000/notify/" or a stateful "http://apprise:8000/notify/{key}".
func NewApprise(url string, opts Options) *Apprise {
	return &Apprise{url: url, client: opts.client()}
}

func (a *Apprise) Name() string { return "apprise" }

// apprisePayload is Apprise's own JSON shape. Its `type` vocabulary happens to
// match [Level] exactly — info/success/warning/failure — which is why Level uses
// those names rather than inventing a parallel set to translate.
type apprisePayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Type  string `json:"type"`
	Tag   string `json:"tag,omitempty"`
}

func (a *Apprise) Send(ctx context.Context, n Notification) error {
	body, err := json.Marshal(apprisePayload{
		Title: n.Title, Body: n.Body,
		Type: string(n.level()),
		Tag:  strings.Join(n.Tags, ","),
	})
	if err != nil {
		return fmt.Errorf("apprise: marshal: %w", err)
	}
	return post(ctx, a.client, a.Name(), a.url,
		bytes.NewReader(body), http.Header{"Content-Type": {"application/json"}})
}

// ── Fan-out ─────────────────────────────────────────────────────────────────

// Multi sends to every sender and joins their errors, so one dead backend does
// not suppress the others. It is the shape a caller with several configured
// destinations wants, and the one they otherwise write by hand with an early
// return that silently drops the rest.
type Multi []Sender

func (m Multi) Name() string { return "multi" }

func (m Multi) Send(ctx context.Context, n Notification) error {
	var errs []error
	for _, s := range m {
		if s == nil {
			continue
		}
		if err := s.Send(ctx, n); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name(), err))
		}
	}
	return joinErrors(errs)
}
