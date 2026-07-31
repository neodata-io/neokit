package httpc

import (
	"context"
	"fmt"
	"net/http"
)

// ImageBlob carries raw image bytes and their content type, for art proxied to a
// browser so the source server's credentials never leave the process.
type ImageBlob struct {
	Data        []byte
	ContentType string
}

// Bytes fetches raw bytes from url with the client's auth applied, returning the
// body and its Content-Type header. It bounds the read at maxBytes
// ([MaxResponseBytes] when <= 0) so a runaway upstream can't exhaust memory,
// and — when a TokenSource is configured — injects a bearer token and retries
// once on a 401, exactly like [BaseClient.DoJSON].
//
// Use it for binary endpoints that DoJSON's JSON decoding doesn't fit; see
// [BaseClient.Image] for the ImageBlob convenience that wraps it.
func (c *BaseClient) Bytes(ctx context.Context, method, url string, maxBytes int64) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = MaxResponseBytes
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
		defer drainClose(resp.Body)
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

// Image fetches art from url and returns it as an [ImageBlob], applying the
// client's auth + 401 retry and bounding the read ([MaxResponseBytes] when
// maxBytes <= 0). A missing Content-Type defaults to image/jpeg.
//
// For a caller holding only a bare *http.Client, a zero BaseClient works:
//
//	blob, err := (&httpc.BaseClient{HTTPClient: hc, Service: "art"}).Image(ctx, u, 0)
//
// Escape every caller-supplied path segment with url.PathEscape before building
// the URL. Go transmits dot-segments verbatim, so an id taken from a request path
// and interpolated raw sends "/Items/../../Users/…" upstream, which the upstream
// normalises into an arbitrary authenticated request made with this client's own
// credentials — whose body is then handed straight back to the caller.
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
