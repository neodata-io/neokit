// Minimal-api is the smallest service built with app: configuration, standard
// middleware, structured errors, and graceful shutdown with one constructor.
package main

import (
	"log"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
)

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
	// config.Base directly, with no struct of its own: a service with no settings
	// beyond the generic ones does not need to declare a type to hold none. Add
	// one the moment you have a field of your own — see production-service.
	cfg, err := config.Load[config.Base]()
	if err != nil {
		return err
	}

	// No Version: an unset one fills itself in from the VCS metadata Go embeds,
	// so this binary still reports its commit in every log line.
	service, err := app.New(app.Options{
		Name: "minimal-api",
		Base: cfg,
	})
	if err != nil {
		return err
	}
	defer service.Close()

	service.HTTP.Get("/hello", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"message": "hello"})
	})

	return service.Run()
}
