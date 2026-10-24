package health_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/neodata-io/neokit/health"
)

func ExampleRegistry() {
	ready := &health.Registry{Log: slog.New(slog.DiscardHandler)}
	ready.Register("database", func(ctx context.Context) error { return nil })
	ready.Register("cache", func(ctx context.Context) error { return errors.New("connection refused") })

	res := ready.Check(context.Background())
	fmt.Println("ready:", res.Ready)
	for _, c := range res.Checks {
		fmt.Printf("%s ok=%t err=%q\n", c.Name, c.OK, c.Err)
	}
	// Output:
	// ready: false
	// database ok=true err=""
	// cache ok=false err="connection refused"
}

// ReadyHandler is for the public port: the status code and a bare verdict, no
// dependency names. Mount DetailHandler behind your own auth for the full sweep.
func ExampleRegistry_ReadyHandler() {
	ready := &health.Registry{Log: slog.New(slog.DiscardHandler)}
	ready.Register("database", func(ctx context.Context) error { return errors.New("down") })

	rec := httptest.NewRecorder()
	ready.ReadyHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	fmt.Println(rec.Code, rec.Body.String())
	// Output: 503 {"ready":false}
}
