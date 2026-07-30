package httpc

import (
	"context"
	"fmt"
	"net/http"
)

// ImageBlob carries raw image bytes and their content type, proxied to the
// browser by the host so the source server's API key never leaves the box. It
// backs every art-proxy method — memory thumbnails, continue-watching posters,
// and request-tile posters alike.
type ImageBlob struct {
	Data        []byte
	ContentType string
}

// MaxImageBytes bounds a single poster/thumbnail fetch. Cover art is small, so
// this ceiling keeps a hostile or misbehaving upstream from streaming unbounded
// bytes into a proxy response. Pass a larger max to [BaseClient.Bytes] for the
// rare oversized asset.
const MaxImageBytes = 8 << 20 // 8 MiB

// Bytes fetches raw bytes from url with the client's auth applied, returning the
// body and its Content-Type header. It bounds the read at maxBytes (MaxImageBytes
// when <= 0) so a runaway upstream can't exhaust memory, and — when a TokenSource
// is configured — injects a bearer token and retries once on a 401, exactly like
// [BaseClient.DoJSON]. Use it for binary endpoints (poster/thumbnail art) that
// DoJSON's JSON decoding doesn't fit; see [BaseClient.Image] for the ImageBlob
// convenience that wraps it.
func (c *BaseClient) Bytes(ctx context.Context, method, url string, maxBytes int64) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = MaxImageBytes
	}

	attempt := func(token string) (status int, data []byte, ct string, err error) {
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return 0, nil, "", fmt.Errorf("%s build request: %w", c.Service, err)
		}
		if c.Auth != nil {
			c.Auth(req)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		// send owns the do/debug/status core (a non-2xx becomes a bounded *APIError,
		// its body already drained); the caller still reads resp.StatusCode to spot a
		// 401 for the token refresh below.
		resp, err := c.send(req, method, url)
		if err != nil {
			if resp != nil {
				return resp.StatusCode, nil, "", err
			}
			return 0, nil, "", err
		}
		defer resp.Body.Close()
		body, err := ReadAllLimited(resp.Body, maxBytes)
		if err != nil {
			return resp.StatusCode, nil, "", fmt.Errorf("%s read body: %w", c.Service, err)
		}
		return resp.StatusCode, body, resp.Header.Get("Content-Type"), nil
	}

	if c.Tokens == nil {
		_, data, ct, err := attempt("")
		return data, ct, err
	}

	// Token path: fetch a valid token, and on a 401 refresh once — passing the
	// rejected token as stale so concurrent 401s collapse into one re-login.
	token, err := c.Tokens.Token(ctx, "")
	if err != nil {
		return nil, "", fmt.Errorf("%s auth: %w", c.Service, err)
	}
	status, data, ct, err := attempt(token)
	if status == http.StatusUnauthorized {
		fresh, ferr := c.Tokens.Token(ctx, token)
		if ferr != nil {
			return nil, "", fmt.Errorf("%s auth: %w", c.Service, ferr)
		}
		_, data, ct, err = attempt(fresh)
	}
	return data, ct, err
}

// FetchImage GETs url with hc and returns the body as an [ImageBlob]: it bounds
// the read at maxBytes (MaxImageBytes when <= 0) and defaults a missing
// Content-Type to image/jpeg. By default it sends no auth — the usual case for
// public cover-art / media-metadata CDNs — but an optional [AuthFunc] can set
// request headers for a server that needs a token. It is the equivalent of
// [BaseClient.Image] for a plugin that talks to art endpoints through a bare
// *http.Client instead of a BaseClient. The caller is responsible for validating
// url first (see a caller-side allowlist check) so the proxy can't be pointed at an arbitrary host.
//
//	// public CDN, no auth:
//	return httpc.FetchImage(ctx, c.http, posterURL, 0)
//	// server that needs a token header:
//	return httpc.FetchImage(ctx, c.http, u, 0, func(r *http.Request) { c.addHeaders(r) })
func FetchImage(ctx context.Context, hc *http.Client, url string, maxBytes int64, auth ...AuthFunc) (*ImageBlob, error) {
	if maxBytes <= 0 {
		maxBytes = MaxImageBytes
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch image: build request: %w", err)
	}
	for _, a := range auth {
		if a != nil {
			a(req)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}
	defer resp.Body.Close()
	if err := CheckStatus("image", resp); err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}
	data, err := ReadAllLimited(resp.Body, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch image: read: %w", err)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return &ImageBlob{Data: data, ContentType: ct}, nil
}

// Image fetches poster/thumbnail art from url and returns it as an [ImageBlob],
// applying the client's auth + 401 retry and bounding the read (MaxImageBytes
// when maxBytes <= 0). A missing Content-Type defaults to image/jpeg. It is the
// one-liner behind a home-screen thumbnail proxy: a plugin returns the blob
// straight to the host, which streams it to the browser from a single origin.
//
//	func (c *client) MediaThumbnail(ctx context.Context, id string, width, height int) (*httpc.ImageBlob, error) {
//	    return c.Image(ctx, c.URL("/Items/%s/Images/Primary?fillWidth=%d&fillHeight=%d",
//	        url.PathEscape(id), width, height), 0)
//	}
//
// Note the url.PathEscape. A thumbnail id reaches a plugin from an open proxy
// route's path param, already URL-decoded, and Go transmits dot-segments
// verbatim — so interpolating it raw sends "/Items/../../Users/…" upstream,
// which the upstream normalises into an arbitrary authenticated request made
// with the plugin's own credentials, and whose body this function then hands
// back to the caller. Escape every caller-supplied path segment.
func (c *BaseClient) Image(ctx context.Context, url string, maxBytes int64) (*ImageBlob, error) {
	data, ct, err := c.Bytes(ctx, http.MethodGet, url, maxBytes)
	if err != nil {
		return nil, err
	}
	if ct == "" {
		ct = "image/jpeg"
	}
	return &ImageBlob{Data: data, ContentType: ct}, nil
}
