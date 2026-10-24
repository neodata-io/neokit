package app_test

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/errs"
)

func exampleGet(a *app.App, path string) {
	resp, err := a.HTTP.Test(httptest.NewRequest(http.MethodGet, path, nil))
	if err != nil {
		log.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(resp.StatusCode, string(body))
}

func Example() {
	a, err := app.New(app.Options{
		Name: "example",
		Base: config.Base{},
		Log:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	// a.API is already mounted at /api/v1.
	a.API.Get("/hello", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"hello": "world"})
	})

	// A service calls a.Run() here; the example sends one request instead.
	exampleGet(a, "/api/v1/hello")
	// Output: 200 {"hello":"world"}
}

// A handler returns domain errors; the errs sentinels map to a status and the
// JSON envelope without any mapper of your own.
func Example_errors() {
	a, err := app.New(app.Options{Name: "example", Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		log.Fatal(err)
	}
	defer a.Close()

	a.API.Get("/items/:id", func(c fiber.Ctx) error {
		return fmt.Errorf("item %s: %w", c.Params("id"), errs.ErrNotFound)
	})

	exampleGet(a, "/api/v1/items/42")
	// Output: 404 {"code":"not_found","error":"not found"}
}
