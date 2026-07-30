package logx

import (
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
	"golang.org/x/term"
)

// Setup configures the global slog logger from the resolved log level and format,
// wraps it so every context-aware line carries the request correlation ID, and
// stamps the app name + build version onto every record (so logs can be
// filtered by build after a deploy, e.g. Loki: `| json | version="1.4.0"`).
//
// The process identity goes under "app", not "service", because "service" is
// already spoken for: callers hand it to per-component loggers, and every
// line about a given component uses it the same way. When the root logger
// also claimed it, each component's record carried two "service" keys —
// `"service":"<process>",…,"service":"<component>"` — and since an
// aggregator keeps whichever it parses last, the process name and the
// component name silently overwrote each other.
//
// format is "auto" (colored console on a TTY, JSON when stderr is redirected —
// Docker/systemd/log aggregators), "text", or "json"; an unknown value falls back
// to the pretty console. An unparseable level warns and defaults to info.
func Setup(app, level, format, version string) {
	// Held, not reported, until the handler below is installed: warning here would
	// emit through slog's bootstrap handler — plain text, no app/version stamp —
	// so the one line explaining why the level is unexpected would be the one line
	// a JSON log pipeline cannot parse.
	logLevel := slog.LevelInfo
	levelErr := logLevel.UnmarshalText([]byte(level))

	isTTY := term.IsTerminal(int(os.Stderr.Fd()))
	if format == "auto" {
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
		"app", app,
		"version", version,
	)
	slog.SetDefault(logger)

	if levelErr != nil {
		slog.Warn("invalid LOG_LEVEL, defaulting to info", "value", level)
	}

	// Announce the level that was actually resolved. printBanner renders only on
	// a TTY, so in a container nothing otherwise reports the effective level —
	// and "why am I seeing DEBUG when LOG_LEVEL=info?" is answerable from the
	// logs themselves rather than from the source. Emitted at info so a
	// deployment that asked for warn or error stays quiet — the confusing case
	// is always an unexpectedly *verbose* logger, which is info or below by
	// definition. The key is "level_configured" because "level" is already the
	// record's own field.
	slog.Info("logging configured", "level_configured", logLevel.String(), "format", format)
}
