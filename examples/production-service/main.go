// Production-service wires an application-owned SQLite database into app's
// readiness and ordered shutdown without introducing a service container.
package main

import (
	"log"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/sqlitex"
)

type Config struct {
	config.Base
}

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
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "./data/production-service.db"
	}

	service, err := app.New(app.Options{
		Name: "production-service",
		Base: cfg.Base,
	})
	if err != nil {
		return err
	}
	defer service.Close()

	db, err := sqlitex.Open(cfg.DatabasePath, nil)
	if err != nil {
		return err
	}
	// Pushed before Run adds its own steps, so the stack unwinds the database
	// after the HTTP drain rather than out from under requests still in flight.
	service.Shutdown.PushCloser("database", db)

	// One declaration, two outputs: a line in the boot report and a /readyz
	// check. The database cannot be called one thing on the console and another
	// in the readiness body.
	service.Declare(app.Subsystem{
		Name:   "database",
		On:     true,
		Detail: cfg.DatabasePath,
		Ready:  db.PingContext,
	})

	service.Fiber.Get("/", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"service": service.Name})
	})

	return service.Run()
}
