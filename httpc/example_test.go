package httpc_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/neodata-io/neokit/errs"
	"github.com/neodata-io/neokit/httpc"
)

type device struct {
	Name string `json:"name"`
	On   bool   `json:"on"`
}

// A client for one upstream API embeds BaseClient and gets JSON, auth, retries
// on idempotent reads, and wrapped errors.
type devices struct{ httpc.BaseClient }

func newDevices(baseURL, token string) *devices {
	return &devices{httpc.NewBaseClient(baseURL, "devices", httpc.BearerAuth(token))}
}

func (c *devices) Get(ctx context.Context, id string) (device, error) {
	var d device
	err := c.DoJSON(ctx, http.MethodGet, c.URL("/devices/%s", url.PathEscape(id)), nil, &d)
	// Wrapping errs.ErrNotFound lets fiberx render a 404 and Classify see it.
	if e, ok := errors.AsType[*httpc.APIError](err); ok && e.StatusCode == http.StatusNotFound {
		return device{}, fmt.Errorf("device %q: %w", id, errs.ErrNotFound)
	}
	return d, err
}

func ExampleBaseClient() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devices/lamp" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"name":"lamp","on":true}`)
	}))
	defer srv.Close()

	c := newDevices(srv.URL, "secret")

	d, err := c.Get(context.Background(), "lamp")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(d.Name, d.On)

	_, err = c.Get(context.Background(), "toaster")
	fmt.Println(errors.Is(err, errs.ErrNotFound), httpc.Classify(err))
	// Output:
	// lamp true
	// true not_found
}

// Fetching a URL a user supplied: the guard refuses private, loopback and
// cloud-metadata addresses at dial time, after DNS has resolved.
func ExampleGuardedTransport() {
	client := httpc.NewHTTPClient(httpc.HTTPOptions{
		Transport: httpc.GuardedTransport(httpc.IsPrivateAddr),
	})
	_, err := client.Get("http://127.0.0.1:9/")
	fmt.Println(err != nil)
	// Output: true
}
