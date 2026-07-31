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

	// Ctx is cancelled once the HTTP server has drained, not when shutdown
	// begins — so a request accepted during the drain can still start background
	// work the drain then waits for. Start background work from it; a long-lived
	// response wants [App.StreamContext], which fires earlier.
	Ctx context.Context

	banner     bool
	cancel     context.CancelFunc
	drain      *drainSignal
	subsystems []Subsystem
}

// drainSignal is the one-shot broadcast that shutdown has started, fired before
// the HTTP server drains. See [App.StreamContext] for what listens to it.
//
// A context rather than a channel this package closes: Run's teardown step and a
// caller's deferred Close can both release it, and a second close panics where a
// second cancel is a no-op — at shutdown, of all moments. It also lets
// StreamContext hang a per-stream cancel off it with [context.AfterFunc].
type drainSignal struct {
	ctx     context.Context
	release context.CancelFunc
}

func newDrainSignal() *drainSignal {
	ctx, cancel := context.WithCancel(context.Background())
	return &drainSignal{ctx: ctx, release: cancel}
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

	a := &App{
		Name: o.Name, Version: o.Version, Cfg: o.Base,
		Log:      log,
		Errors:   fiberx.NewErrors(o.ErrorMapper),
		Shutdown: &lifecycle.Stack{Log: log},
		Health:   health.New(),
		Ctx:      ctx,
		banner:   banner,
		cancel:   cancel,
		drain:    newDrainSignal(),
	}
	a.Errors.Log = log
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
	a.Declare(diagnosticsSubsystem(a.diagnosticsAddr(), o.Base.EnablePprof))

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

// diagnosticsSubsystem is the report line for the diagnostics listener.
//
// The two facts worth stating are the two that are otherwise invisible. That
// port binds loopback by default, so a scrape from another container fails with
// nothing wrong on this side to find; and whether pprof — which will hand over a
// heap dump — is mounted is recorded nowhere else the operator can see. Neither
// should have to be inferred from a default.
func diagnosticsSubsystem(addr string, pprof bool) Subsystem {
	mounted := "metrics, health"
	if pprof {
		mounted += ", pprof"
	}
	return Subsystem{Name: "diagnostics", On: true, Detail: addr + " · " + mounted}
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

// StreamContext returns the context a long-lived response should select on —
// Server-Sent Events, a WebSocket, an NDJSON feed. It is cancelled *before* the
// HTTP server drains, so the stream ends at the start of shutdown instead of
// holding the drain open for its full timeout — which would turn a clean SIGTERM
// into a deadline error and a non-zero exit.
//
// Fiber drains by waiting for every connection to go idle, and a stream that is
// still streaming never does. Nothing inside the drain can end such a handler; it
// has to be told out of band, first. That is also why [App.Ctx] cannot serve as
// this signal — Ctx is cancelled after the drain, so that a request accepted
// during it can still start background work.
//
// It derives from the request context, so the trace span opened for the request
// carries into the stream. Note that Fiber's request context is not cancelled
// when the client disconnects — a failed write is still how you learn that.
//
// Call cancel when the stream ends, from inside the stream body rather than the
// handler: with SetBodyStreamWriter the handler returns before the stream
// begins, so a cancel deferred in the handler would sever it immediately. Not
// calling it retains a registration on the drain signal until the process exits.
//
//	func (h *Handler) Events(c fiber.Ctx) error {
//		ctx, cancel := h.app.StreamContext(c)
//		c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
//			defer cancel()
//			for {
//				select {
//				case ev := <-events:
//					// ... write and flush; a write error means the client is gone
//				case <-ctx.Done():
//					return
//				}
//			}
//		})
//		return nil
//	}
//
// The drain signal itself is deliberately unexported. A handler selects on an
// ordinary context and never sees a neokit type, and background work started
// from [App.Ctx] wants the later signal anyway — so there is nothing left for a
// raw channel to serve.
func (a *App) StreamContext(c fiber.Ctx) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.Context())
	// AfterFunc rather than a goroutine per stream, and stop() rather than
	// leaving it registered: nothing cancels a Fiber request context on its own,
	// so without releasing this a busy service accumulates one registration on
	// the drain signal per stream it has ever served.
	stop := context.AfterFunc(a.drain.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// closeDraining releases the drain signal. Run's teardown step and a caller's
// deferred Close can both reach it; a second release is a no-op.
func (a *App) closeDraining() { a.drain.release() }
