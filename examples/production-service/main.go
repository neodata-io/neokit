// Production-service is the batteries-included layer: neokit.New, then one call
// per feature. a.Database is the handle, the report line, the /readyz check and
// the shutdown step — without a service container.
package main

import (
	"log"

	"github.com/gofiber/fiber/v3"

	neokit "github.com/neodata-io/neokit"
	"github.com/neodata-io/neokit/app"
	"github.com/neodata-io/neokit/config"
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

	service, err := neokit.New(app.Options{
		Name: "production-service",
		Base: cfg.Base,
	})
	if err != nil {
		return err
	}
	defer service.Close()

	// One call, four outputs: the handle, a line in the boot report, a /readyz
	// check, and a place in the shutdown order — before Run adds its own steps,
	// so the database closes after the HTTP drain rather than out from under
	// requests still in flight.
	db, err := service.Database(cfg.DatabasePath, nil)
	if err != nil {
		return err
	}

	// db is an ordinary *sql.DB. Database registered it, but it handed it back rather
	// than hiding it — so a handler reaches it through its own constructor, and
	// forgetting to wire one is a compile error rather than a runtime surprise.
	service.HTTP.Get("/", func(c fiber.Ctx) error {
		var version string
		if err := db.QueryRowContext(c.Context(), "SELECT sqlite_version()").Scan(&version); err != nil {
			return err
		}
		return c.JSON(fiber.Map{"service": service.Name, "sqlite": version})
	})

	return service.Run()
}
