package httpc

import (
	"context"
	"net/http"
)

// ImageBlob carries raw image bytes and their content type — for an image
// proxied to a browser, so the source server's credentials never leave the
// process.
type ImageBlob struct {
	Data        []byte
	ContentType string
}

// Image fetches url and returns it as an [ImageBlob], applying the client's
// auth + 401 retry and bounding the read ([MaxResponseBytes] when maxBytes
// <= 0). A missing Content-Type defaults to image/jpeg.
//
// For a caller holding only a bare *http.Client, a zero BaseClient works:
//
//	blob, err := (&httpc.BaseClient{HTTPClient: hc, Service: "covers"}).Image(ctx, u, 0)
//
// See [BaseClient.URL] on escaping caller-supplied path segments.
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
