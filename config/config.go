// Package config holds the environment settings every service has, and the one
// call that parses them.
//
// It is deliberately only the *generic* half. An application embeds [Base] and
// adds its own fields, so there is one struct, one parse, and one error path —
// rather than a neokit config and an application config that can disagree about
// which file was loaded.
package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Base is the settings that are the same in every service.
//
// DatabasePath is carried as a value only: nothing in neokit opens a database.
// Which store an application uses — or whether it has one at all — is its own
// decision, and a builder that assumed SQLite would be wrong for the next
// project.
type Base struct {
	// Port is the application listener's port.
	Port int `env:"PORT" envDefault:"8080"`

	// BindAddr is the interface to listen on. Empty binds all IPv4 interfaces;
	// set it to "127.0.0.1" to keep an unauthenticated API on loopback behind a
	// reverse proxy that adds auth.
	BindAddr string `env:"BIND_ADDR"`

	// LogLevel is debug|info|warn|error.
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// LogFormat selects the slog handler: "auto" (colored console on a TTY, JSON
	// when redirected), or "text"/"json" to force one.
	LogFormat string `env:"LOG_FORMAT" envDefault:"auto"`

	// CorsOrigins is the browser origins allowed to call the API.
	//
	// The localhost default is a development convenience so a fresh checkout
	// works with a local frontend. Set CORS_ORIGINS explicitly in production —
	// this default is not a security boundary, it is a convenience.
	CorsOrigins []string `env:"CORS_ORIGINS" envDefault:"http://localhost:3000" envSeparator:","`

	// MetricsPort is the diagnostics listener (metrics, health, optional pprof).
	// Bind it to loopback in production and reach it through a tunnel.
	MetricsPort int `env:"METRICS_PORT" envDefault:"9090"`

	// DatabasePath is passed through to whatever store the application opens.
	DatabasePath string `env:"DATABASE_PATH"`
}

// Load reads a .env file if one is present, then parses T from the environment.
//
// T is normally a struct embedding [Base]. A missing .env is not an error: it is
// a development convenience, and in a container the values come from the
// environment itself.
func Load[T any]() (T, error) {
	// Best-effort: env vars already set always win, so this only fills gaps.
	_ = godotenv.Load()

	cfg, err := env.ParseAs[T]()
	if err != nil {
		var zero T
		// env's error names the offending field and the value it could not
		// parse, which is what a developer needs; wrapping says where it came
		// from. Deliberately NOT reflected over to recover the env var name —
		// mapping Port to PORT is trivial for a reader, and reflection here
		// would be exactly the magic this library exists to avoid.
		return zero, fmt.Errorf("parse environment: %w", err)
	}
	return cfg, nil
}
