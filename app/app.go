// Package app is neokit's application builder: the boot sequence every service
// would otherwise retype, performed once in a documented order.
//
// It is deliberately *not* a container. Every dependency it constructs is an
// exported field on [App], and a handler still receives what it needs through
// its own constructor. There is no lookup, no reflection, and no bespoke handler
// signature — you write ordinary Fiber handlers against ordinary types.
//
// The trade it makes is that neokit chooses the boot *order*. An application
// that needs a different one ignores [New] and wires from the same packages by
// hand; that escape hatch is what lets this be opinionated.
//
//	a, err := app.New(app.Options{Name: "okstables", Version: version, Base: cfg.Base})
//	if err != nil { return err }
//	defer a.Close()
//
//	store, err := sqlitex.Open(cfg.DatabasePath, migrate)
//	a.Shutdown.PushCloser("database", store)
//	a.Declare(app.Subsystem{Name: "database", On: true, Detail: cfg.DatabasePath, Ready: store.PingContext})
//
//	return a.Run()
package app

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/compress"
	"github.com/gofiber/fiber/v3/middleware/cors"
	recovermw "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"

	"github.com/neodata-io/neokit/config"
	"github.com/neodata-io/neokit/fiberx"
	"github.com/neodata-io/neokit/health"
	"github.com/neodata-io/neokit/lifecycle"
	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/metrics"
	"github.com/neodata-io/neokit/tracing"
)

// Default request bounds. WriteTimeout is deliberately absent: a Server-Sent
// Events stream lives for as long as its subject does, and a write deadline
// would sever it.
const (
	defaultBodyLimit   = 1 << 20 // 1 MiB
	defaultReadTimeout = 15 * time.Second
	defaultIdleTimeout = 120 * time.Second
)

// Options configures [New]. Only Name is required.
type Options struct {
	// Name identifies the service in logs, traces and metrics. Required — an
	// anonymous process is anonymous in three systems at once.
	Name string

	// Version is stamped alongside Name. Usually buildinfo.Get().Version.
	Version string

	// Base is the parsed generic configuration. See [config.Base].
	Base config.Base

	// ErrorMapper maps the application's own error sentinels to an HTTP status,
	// public message and code. Nil means only the generic vocabulary applies.
	ErrorMapper fiberx.DomainMapper

	// QuietPaths silences the request log for routes that are pure noise — a
	// health check or a status sweep something polls on a timer.
	QuietPaths func(path string) bool

	// Fiber overrides the default configuration. Only the fields set here are
	// applied over the defaults.
	Fiber *fiber.Config

	// Log replaces the logger. Nil configures one from Base via logx.
	Log *slog.Logger

	// Banner prints the boot report at the top of Run. Nil means true.
	Banner *bool
}

// App is a constructed application. Every dependency is an exported field.
type App struct {
	Name    string
	Version string
	Cfg     config.Base

	Log      *slog.Logger
	Fiber    *fiber.App
	Errors   *fiberx.Errors
	Shutdown *lifecycle.Stack
	Health   *health.Registry

	// Ctx is cancelled when shutdown begins. Start background work from it.
	Ctx context.Context

	// Draining is closed *before* the HTTP server drains, so a long-lived
	// handler (Server-Sent Events, a WebSocket) can return instead of holding
	// the drain open for its full timeout — which would turn a clean SIGTERM
	// into a deadline error and a non-zero exit.
	Draining <-chan struct{}

	banner   bool
	cancel   context.CancelFunc
	draining chan struct{}
	// drainOnce guards closeDraining: Run's teardown step and a caller's
	// deferred Close can both reach it, and closing a closed channel panics —
	// at shutdown, of all moments.
	drainOnce sync.Once

	mu         sync.RWMutex
	subsystems []Subsystem
}

// New performs the boot sequence and returns the constructed application.
//
// The order is fixed: logging, the shutdown stack, tracing, metrics export, the
// application context, the HTTP server and its middleware, then health. It is
// documented here rather than re-derived per project.
//
// New starts no listener. Register routes, declare subsystems, push your own
// teardown steps, then call [App.Run].
func New(o Options) (*App, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("app: Options.Name is required")
	}

	log := o.Log
	if log == nil {
		logx.Setup(o.Name, o.Base.LogLevel, o.Base.LogFormat, o.Version)
		log = slog.Default()
	}

	banner := true
	if o.Banner != nil {
		banner = *o.Banner
	}

	ctx, cancel := context.WithCancel(context.Background())
	draining := make(chan struct{})

	a := &App{
		Name: o.Name, Version: o.Version, Cfg: o.Base,
		Log:      log,
		Errors:   fiberx.NewErrors(o.ErrorMapper),
		Shutdown: &lifecycle.Stack{Log: log},
		Health:   health.New(),
		Ctx:      ctx,
		Draining: draining,
		banner:   banner,
		cancel:   cancel,
		draining: draining,
	}
	if o.QuietPaths != nil {
		a.Errors.QuietPath = o.QuietPaths
	}

	// Tracing and metrics export are both opt-in on the standard OTEL env vars
	// and are no-ops without them. They are pushed first, so they flush last —
	// a span emitted by any later teardown step still gets exported.
	traceShutdown, err := tracing.Init(ctx, tracing.Config{ServiceName: o.Name, Version: o.Version})
	if err != nil {
		cancel()
		return nil, err
	}
	a.Shutdown.Push("tracing", traceShutdown)
	a.Declare(otelSubsystem("tracing"))

	metricShutdown, err := metrics.Init(ctx, metrics.Config{ServiceName: o.Name, Version: o.Version})
	if err != nil {
		cancel()
		return nil, err
	}
	a.Shutdown.Push("metrics-export", metricShutdown)
	a.Declare(otelSubsystem("metrics export"))

	a.Fiber = a.newFiber(o)
	return a, nil
}

// otelSubsystem reports whether an OpenTelemetry signal is exporting, reading
// the same env var its SDK does so the report cannot disagree with reality.
func otelSubsystem(name string) Subsystem {
	endpoint := strings.TrimSpace(osGetenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		return Subsystem{Name: name, On: false, Detail: "OTEL_EXPORTER_OTLP_ENDPOINT unset"}
	}
	return Subsystem{Name: name, On: true, Detail: endpoint}
}

// newFiber builds the HTTP server and the standard middleware chain.
func (a *App) newFiber(o Options) *fiber.App {
	cfg := fiber.Config{
		AppName:   a.Name,
		BodyLimit: defaultBodyLimit,
		// Bound how long a client may take to send a request and how long an idle
		// keep-alive lingers, so a slow or stuck client cannot pin a connection.
		ReadTimeout: defaultReadTimeout,
		IdleTimeout: defaultIdleTimeout,
		// One renderer, so every returned error crosses the wire as the same
		// envelope. Without it Fiber emits plain text a client cannot parse.
		ErrorHandler: a.Errors.Render,
	}
	if o.Fiber != nil {
		cfg = mergeFiberConfig(cfg, *o.Fiber)
	}
	f := fiber.New(cfg)

	// Recover first, so it wraps everything below it. A panic without a stack
	// trace is a bare 500 and the least information at exactly the wrong moment.
	f.Use(recovermw.New(recovermw.Config{
		EnableStackTrace: true,
		StackTraceHandler: func(c fiber.Ctx, e any) {
			a.Log.LogAttrs(c.Context(), slog.LevelError, "panic recovered",
				slog.Any("panic", e),
				slog.String("method", c.Method()),
				slog.String("path", c.Path()),
				slog.String("stack", string(debug.Stack())),
			)
		},
	}))
	f.Use(requestid.New())
	// After requestid so both ids exist, before the logger so its summary line
	// carries the trace id.
	f.Use(tracing.Middleware())
	f.Use(a.Errors.MetricsAndLogger())
	// Compress everything except Server-Sent Events: a compressor buffers the
	// writes an event stream exists to flush immediately. EventSource always
	// sends this Accept header, so keying off it leaves SSE uncompressed wherever
	// the route is mounted.
	f.Use(compress.New(compress.Config{
		Next: func(c fiber.Ctx) bool {
			return strings.Contains(c.Get("Accept"), "text/event-stream")
		},
	}))
	f.Use(cors.New(cors.Config{
		AllowOrigins: a.Cfg.CorsOrigins,
		AllowHeaders: []string{"Origin", "Content-Type", "Accept"},
		AllowMethods: []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
	}))
	return f
}

// mergeFiberConfig applies the caller's non-zero overrides over the defaults.
func mergeFiberConfig(base, over fiber.Config) fiber.Config {
	if over.AppName != "" {
		base.AppName = over.AppName
	}
	if over.BodyLimit != 0 {
		base.BodyLimit = over.BodyLimit
	}
	if over.ReadTimeout != 0 {
		base.ReadTimeout = over.ReadTimeout
	}
	if over.WriteTimeout != 0 {
		base.WriteTimeout = over.WriteTimeout
	}
	if over.IdleTimeout != 0 {
		base.IdleTimeout = over.IdleTimeout
	}
	if over.ErrorHandler != nil {
		base.ErrorHandler = over.ErrorHandler
	}
	return base
}

// closeDraining closes the channel at most once. Run's teardown step and Close
// can both reach it, and closing a closed channel panics — at shutdown, of all
// moments.
func (a *App) closeDraining() {
	a.drainOnce.Do(func() { close(a.draining) })
}
