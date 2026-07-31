// External-client calls a fixed upstream with a bounded timeout, idempotent
// retry policy, optional tracing, and structured errors.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/neodata-io/neokit/httpc"
)

type Status struct {
	State string `json:"state"`
}

func main() {
	baseURL := strings.TrimSpace(os.Getenv("UPSTREAM_URL"))
	if baseURL == "" {
		log.Fatal("UPSTREAM_URL is required")
	}

	client := httpc.NewBaseClient(
		baseURL,
		"upstream",
		httpc.BearerAuth(os.Getenv("UPSTREAM_TOKEN")),
	)
	client.HTTPClient = httpc.NewHTTPClient(httpc.HTTPOptions{
		Timeout: 5 * time.Second,
		Tracing: true,
	})

	var status Status
	if err := client.DoJSON(context.Background(), http.MethodGet, client.URL("/v1/status"), nil, &status); err != nil {
		log.Fatal(err)
	}
	log.Printf("upstream state: %s", status.State)
}
