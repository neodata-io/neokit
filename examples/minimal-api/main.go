// Minimal-api is the smallest service built with app: configuration, standard
// middleware, structured errors, and graceful shutdown with one constructor.
package main

import (
	"log"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
)

type Config struct {
	config.Base
}

// main does nothing but choose the exit code. The work is in run, because
// log.Fatal calls os.Exit, and os.Exit does not run deferred functions: a
// `defer service.Close()` in main would be skipped on exactly the error paths
// it is there to cover.
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load[Config]()
	if err != nil {
		return err
	}

	service, err := app.New(app.Options{
		Name: "minimal-api",
		Base: cfg.Base,
	})
	if err != nil {
		return err
	}
	defer service.Close()

	service.Fiber.Get("/hello", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"message": "hello"})
	})

	return service.Run()
}
