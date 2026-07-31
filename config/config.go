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

	// MetricsPort is the diagnostics listener (metrics and health).
	MetricsPort int `env:"METRICS_PORT" envDefault:"9090"`

	// MetricsBindAddr is the interface for the diagnostics listener. It defaults
	// to [DefaultMetricsBindAddr] because readiness responses and metrics are
	// operational detail, not a public API. A scrape from another container
	// needs "0.0.0.0" here.
	MetricsBindAddr string `env:"METRICS_BIND_ADDR" envDefault:"127.0.0.1"`

	// EnablePprof mounts profiling endpoints on the diagnostics listener. It is
	// disabled by default because profiles can contain sensitive process data.
	EnablePprof bool `env:"ENABLE_PPROF" envDefault:"false"`

	// DatabasePath is passed through to whatever store the application opens.
	DatabasePath string `env:"DATABASE_PATH"`
}

// DefaultMetricsBindAddr is where the diagnostics listener binds when
// METRICS_BIND_ADDR is unset. It is exported so a caller that builds a [Base] in
// code rather than from the environment resolves the empty value the same way,
// instead of falling back to every interface. A struct tag cannot reference a
// constant, so the tag above repeats the literal; the config tests hold the two
// together.
const DefaultMetricsBindAddr = "127.0.0.1"

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
