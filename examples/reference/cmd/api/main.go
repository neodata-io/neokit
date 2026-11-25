// Reference is a complete small service in the layout neokit is meant for. Copy
// the directory, rename the module path, and replace tasks with your own
// domain; see docs/architecture.md for what goes where and why.
//
//	cmd/api/                    composition root — the only place that knows every package
//	internal/domain/            what a task is; imports nothing of the service
//	internal/app/tasks/         use cases, and the ports they declare
//	internal/adapter/sqlite/    one Store implementing those ports
//	internal/adapter/http/      one package per module, each with one Register
//
// Dependencies point inward only: adapters import app and domain, app imports
// domain, domain imports neither. Nothing imports cmd.
//
// Run it from the repository root:
//
//	go run ./examples/reference/cmd/api
//	curl -s -X POST localhost:8080/api/v1/tasks -d '{"title":"write the report"}' -H 'Content-Type: application/json'
//
// It starts open. Set the OIDC_* variables below and the same routes require a
// sign-in, with nothing else changed.
package main

import (
	"log"

	"github.com/neodata-io/neokit/config"
)

// Config is the service's settings: neokit's generic ones, and its own beside
// them in the same struct, so there is one parse and one error path.
type Config struct {
	config.Base

	// The login. All four empty leaves the service open — see wire.
	OIDCIssuer       string `env:"OIDC_ISSUER"`
	OIDCClientID     string `env:"OIDC_CLIENT_ID"`
	OIDCClientSecret string `env:"OIDC_CLIENT_SECRET"`
	// OIDCBaseURL is this service's own public address, which the identity
	// provider redirects back to.
	OIDCBaseURL string `env:"OIDC_BASE_URL"`
	// OIDCOwnerGroup is the provider group whose members are owners.
	OIDCOwnerGroup string `env:"OIDC_OWNER_GROUP" envDefault:"admin"`
}

// main does nothing but choose the exit code. The work is in run, because
// log.Fatal calls os.Exit, and os.Exit does not run deferred functions.
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load[Config](".env")
	if err != nil {
		return err
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "./data/reference.db"
	}

	svc, err := wire(cfg)
	if err != nil {
		return err
	}
	// Covers a failure between here and Run; Run tears down on the normal path
	// and Close is idempotent.
	defer svc.app.Close()

	return svc.app.Run()
}
