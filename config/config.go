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
	"reflect"

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
		// env's own error already names the offending variable; wrapping keeps
		// that and says where it came from.
		errWithVar := enrichErrorWithEnvVar(err, reflect.TypeOf(cfg))
		return zero, fmt.Errorf("parse environment: %w", errWithVar)
	}
	return cfg, nil
}

// enrichErrorWithEnvVar enhances an env parsing error to include the environment
// variable name (from the struct tag) alongside the field name for clarity.
func enrichErrorWithEnvVar(err error, typ reflect.Type) error {
	if err == nil || typ == nil {
		return err
	}

	errStr := err.Error()
	if !contains(errStr, `field "`) {
		return err
	}

	// Extract the field name from the error message.
	fieldName := extractFieldName(errStr)
	if fieldName == "" {
		return err
	}

	// Look up the env var name from the struct tags.
	envVarName := findEnvTag(typ, fieldName)
	if envVarName == "" {
		return err
	}

	// If the error doesn't already mention the env var, add it.
	if !contains(errStr, envVarName) {
		return fmt.Errorf("%s (env var %s)", err, envVarName)
	}
	return err
}

// extractFieldName pulls the struct field name from an env error like
// `field "FieldName"`.
func extractFieldName(errStr string) string {
	start := indexOf(errStr, `field "`)
	if start < 0 {
		return ""
	}
	start += len(`field "`)
	end := indexOf(errStr[start:], `"`)
	if end < 0 {
		return ""
	}
	return errStr[start : start+end]
}

// findEnvTag searches a struct type for the field name and returns its
// env tag value, if present. It recursively checks embedded structs.
func findEnvTag(typ reflect.Type, fieldName string) string {
	if typ == nil {
		return ""
	}

	// Handle pointer types.
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	if typ.Kind() != reflect.Struct {
		return ""
	}

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Name == fieldName {
			if tag, ok := f.Tag.Lookup("env"); ok {
				return tag
			}
		}
		// Recursively check embedded structs.
		if f.Anonymous {
			if tag := findEnvTag(f.Type, fieldName); tag != "" {
				return tag
			}
		}
	}
	return ""
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
