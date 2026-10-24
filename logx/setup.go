package logx

import (
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
	"golang.org/x/term"
)

// Options configures [New] and [Setup].
type Options struct {
	// App and Version are stamped on every record. The key is "app", not
	// "service": components use "service" for themselves, and two would collide.
	App     string
	Version string

	// Level is debug|info|warn|error. Empty means info; anything else warns and
	// falls back to info.
	Level string

	// Format is "auto" (colored console on a TTY, JSON when stderr is redirected),
	// "text" or "json". Empty means auto; an unknown value gets the console.
	Format string
}

// New builds the logger neokit uses: leveled, writing to stderr, stamped with
// the app and version, and carrying the request ID from the context. It leaves
// slog.Default alone — see [Setup] for that.
func New(o Options) *slog.Logger {
	logLevel := slog.LevelInfo
	var levelErr error
	if o.Level != "" {
		levelErr = logLevel.UnmarshalText([]byte(o.Level))
	}

	format := o.Format
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))
	if format == "" || format == "auto" {
		if isTTY {
			format = "text"
		} else {
			format = "json"
		}
	}

	var handler slog.Handler
	switch format {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	default: // "text" and any unknown value fall back to the pretty console.
		handler = tint.NewTextHandler(os.Stderr, &tint.Options{
			Level:      logLevel,
			TimeFormat: "15:04:05.000",
			NoColor:    !isTTY, // never emit ANSI codes into a redirected stream
		})
	}
	logger := slog.New(NewContextHandler(handler)).With(
		"app", o.App,
		"version", o.Version,
	)

	// Reported through the new logger, so even this line is in the pipeline's format.
	if levelErr != nil {
		logger.Warn("invalid LOG_LEVEL, defaulting to info", "value", o.Level)
	}
	// In a container nothing else reports the effective level.
	logger.Info("logging configured", "level_configured", logLevel.String(), "format", format)
	return logger
}

// Setup is [New] installed as slog.Default, for a process that owns its global
// logger. It returns the logger too.
func Setup(o Options) *slog.Logger {
	logger := New(o)
	slog.SetDefault(logger)
	return logger
}
