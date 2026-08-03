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

	// One call, four outputs: the handle, a line in the boot report, a /readyz
	// check, and a place in the shutdown order — declared before Run adds its own
	// steps, so the database closes after the HTTP drain rather than out from
	// under requests still in flight.
	db, err := sqlitex.Open(service, cfg.DatabasePath, nil)
	if err != nil {
		return err
	}

	// db is an ordinary *sql.DB. Open registered it, but it handed it back rather
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
