// Package app is neokit's application builder: the boot sequence every service
// would otherwise retype, performed once in a documented order.
//
// neokit builds on Fiber v3. [App.HTTP] is a *fiber.App and handlers are
// ordinary Fiber handlers — a deliberate choice, not an implementation detail
// waiting to be abstracted away.
//
// It is deliberately *not* a container. What it constructs is an exported field
// on [App], and a handler still receives what it needs through its own
// constructor. There is no lookup, no reflection, and no bespoke handler
// signature — you write ordinary Fiber handlers against ordinary types.
//
// What it fixes is narrow: where its own four teardown steps sit. Streams and
// the HTTP drain run before yours, the OpenTelemetry flush after them, because
// those positions are what make a SIGTERM exit clean — each is pinned by a test.
// Your own steps, and their order among themselves, are yours: push them onto
// [App.Shutdown] and they unwind in reverse. So are routes, Fiber config, error
// mapping and the logger.
//
//	a, err := app.New(app.Options{Name: "okstables", Version: version, Base: cfg.Base})
//	if err != nil { return err }
//	defer a.Close()
//
//	store, err := sqlitex.Open(cfg.DatabasePath, migrate)
//	a.Declare(app.Subsystem{
//		Name: "database", On: true, Detail: cfg.DatabasePath,
//		Ready: store.PingContext, Close: lifecycle.Closer(store),
//	})
//
//	return a.Run()
package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"github.com/gofiber/fiber/v3/middleware/compress"
	"github.com/gofiber/fiber/v3/middleware/cors"
	recovermw "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"

	"github.com/neodata-io/neokit/buildinfo"
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

// The probe endpoints, on the application listener.
//
// Exported because their reachability is the deployment's problem rather than a
// second listener's: a container health check and a reverse-proxy deny rule both
// have to name them, and the same literal typed into a compose file, an ingress
// and a test is how one of the three ends up stale.
//
// Both answer with a verdict and no more — [App.ReadyDetail] is where the
// per-check detail lives, for a route you have put behind your own
// authentication. The split is deliberate: an orchestrator reads only the status
// code, so naming your dependencies and their errors here would give away a map
// of the infrastructure to buy nothing.
const (
	LivePath  = "/healthz"
	ReadyPath = "/readyz"
)

// MetricsPath is where the Prometheus endpoint is served. Always mounted, at the
// conventional path, so a scrape config that assumes it works without being told
// — see [config.Base.MetricsToken] for the part you may want to set.
const MetricsPath = "/metrics"

// Options configures [New]. Only Name is required.
type Options struct {
	// Name identifies the service in logs, traces and metrics. Required — an
	// anonymous process is anonymous in three systems at once.
	Name string

	// Version is stamped alongside Name in the boot report, every log line,
	// every span and the metrics resource.
	//
	// Leave it empty and it fills itself in. `go build` embeds VCS metadata in any
	// binary built from a work tree, so an unset version becomes
	// "dev (a1b2c3d, dirty)" — more than most services manage with a Makefile.
	// (`go run` stamps nothing, so it reports a bare "dev"; that is the toolchain's
	// behaviour, not a fallback of ours.) Pass your own once you have release
	// builds to stamp:
	//
	//	Version: buildinfo.Get(version, commit, date).String(),
	//
	// where the three are your own package's ldflags variables. See [buildinfo]
	// for why they have to live in your package rather than in neokit's.
	Version string

	// Base is the parsed generic configuration. See [config.Base].
	Base config.Base

	// Fiber adjusts the HTTP config after neokit fills in its defaults. A
	// function, not a *fiber.Config to merge: a zero field is indistinguishable
	// from one set to false, so any merge silently drops most of its input.
	//
	//	Fiber: func(c *fiber.Config) { c.TrustProxy = true },
	Fiber func(*fiber.Config)

	// Log replaces the logger. Nil configures one from Base via logx.
	Log *slog.Logger
}

// App is a constructed application. Everything a caller wires against is an
// exported field; the boot machinery behind [App.Declare] is not.
type App struct {
	Name    string
	Version string
	Cfg     config.Base

	Log      *slog.Logger
	HTTP     *fiber.App
	Errors   *fiberx.Errors
	Shutdown *lifecycle.Stack

	ctx    context.Context
	cancel context.CancelFunc
	drain  *drainSignal
	// Unexported so it can only be fed by Declare, which also writes the boot
	// report — a check registered around it would be invisible there.
	health *health.Registry
	// metrics serves the Prometheus endpoint, or is nil when it is switched off.
	// Unexported because where it is mounted is the builder's decision:
	// above the middleware chain, so a scrape is not itself logged and metered.
	metrics    http.Handler
	subsystems []Subsystem
}

// drainSignal broadcasts that shutdown has started, before the HTTP drain.
//
// A context, not a channel: Run's teardown and a deferred Close can both release
// it, and a second cancel is a no-op where a second close panics.
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
// application context, then the HTTP server with its probe routes and
// middleware. It is documented here rather than re-derived per project.
//
// New starts no listener. Register routes, declare subsystems, push your own
// teardown steps, then call [App.Run].
func New(o Options) (*App, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("app: Options.Name is required")
	}

	// Fill the version rather than leave it blank. Go embeds the commit, the build
	// time and whether the tree was dirty into any binary built from a work tree,
	// so this costs the caller nothing and is the difference between a log line
	// that says which build produced it and one that does not. A version the
	// caller did supply is used exactly as given — this fills, it never overrides.
	if strings.TrimSpace(o.Version) == "" {
		o.Version = buildinfo.Get("", "", "").String()
	}

	log := o.Log
	if log == nil {
		logx.Setup(o.Name, o.Base.LogLevel, o.Base.LogFormat, o.Version)
		log = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	a := &App{
		Name: o.Name, Version: o.Version, Cfg: o.Base,
		Log:      log,
		Errors:   &fiberx.Errors{Log: log},
		Shutdown: &lifecycle.Stack{Log: log},
		health:   &health.Registry{Log: log},
		ctx:      ctx,
		cancel:   cancel,
		drain:    newDrainSignal(),
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

	pipeline, err := metrics.Init(ctx, metrics.Config{
		ServiceName: o.Name, Version: o.Version, Pull: true,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	a.metrics = pipeline.Handler
	a.Shutdown.Push("metrics-export", pipeline.Shutdown)
	a.Declare(otelSubsystem("metrics export"))
	a.Declare(metricsSubsystem(a.metrics != nil, o.Base.MetricsToken))
	a.Declare(healthSubsystem())

	a.HTTP = a.newFiber(o)
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

// metricsSubsystem is the report line for the Prometheus endpoint.
//
// Whether it is authenticated is the fact worth stating: unauthenticated is the
// default and the right answer on a private network, but it is also the one
// setting whose absence is completely silent — the endpoint answers either way,
// and nothing else in the process will ever mention it again.
func metricsSubsystem(mounted bool, token string) Subsystem {
	if !mounted {
		// Only reachable when the Prometheus reader failed to build, which
		// metrics.Init has already logged. Reported rather than hidden: the
		// endpoint answering 404 is otherwise indistinguishable from a typo in
		// whatever is trying to scrape it.
		return Subsystem{Name: "metrics endpoint", On: false, Detail: "reader unavailable — see the log above"}
	}
	guard := "unauthenticated · set METRICS_TOKEN to require a bearer token"
	if token != "" {
		guard = "bearer token required"
	}
	return Subsystem{Name: "metrics endpoint", On: true, Detail: MetricsPath + " · " + guard}
}

// bearerGuard requires `Authorization: Bearer <token>`, or passes everything
// through when token is empty.
//
// The comparison is constant-time. A token check that returns as soon as it
// finds a wrong byte leaks the correct prefix through its own timing, and
// recovering a secret one byte at a time from a remote endpoint is a published
// technique rather than a theoretical one.
func bearerGuard(token string) fiber.Handler {
	if token == "" {
		return func(c fiber.Ctx) error { return c.Next() }
	}
	want := []byte("Bearer " + token)
	return func(c fiber.Ctx) error {
		got := []byte(c.Get(fiber.HeaderAuthorization))
		// ConstantTimeCompare is documented to return 0 on a length mismatch
		// without comparing, so the length itself is not protected. That is fine —
		// the length of a token is not the secret — but it does mean the check
		// cannot be skipped for equal-length-only inputs.
		if subtle.ConstantTimeCompare(got, want) != 1 {
			return fiber.NewError(fiber.StatusUnauthorized, "unauthorized")
		}
		return c.Next()
	}
}

// healthSubsystem is the report line for the probe endpoints.
//
// Worth a line because their location is the one thing about them an operator
// cannot guess and cannot see anywhere else: they are on the application
// listener, which means they are reachable by anyone who can reach the API, and
// [config.Base.BindAddr] is the only thing that narrows that.
func healthSubsystem() Subsystem {
	return Subsystem{Name: "health", On: true, Detail: LivePath + ", " + ReadyPath}
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
		o.Fiber(&cfg)
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
	// Before the rest of the chain, and that placement is the whole point: Fiber
	// walks its stack in registration order, so a probe matches here and returns
	// without ever reaching the middleware below. Probe traffic is the highest
	// volume and lowest information a service sees — a 10-second liveness interval
	// is 8 640 log lines a day, and counting it in http.server.request.duration
	// drags every latency percentile toward the cost of answering `{"status":"ok"}`.
	//
	// Recover is above this line, so a readiness check that panics is still a 500
	// rather than a dead process. TestProbesBypassTheRequestLog pins the ordering.
	f.Get(LivePath, adaptor.HTTPHandler(health.LiveHandler()))
	f.Get(ReadyPath, adaptor.HTTPHandler(a.health.ReadyHandler()))

	// The scrape belongs up here for the same reasons and one more: measured by
	// the middleware below, every scrape would add a data point about the act of
	// collecting data points, and at a 15-second interval that self-observation
	// is a visible fraction of the traffic on an otherwise idle service.
	if a.metrics != nil {
		f.Get(MetricsPath, bearerGuard(o.Base.MetricsToken), adaptor.HTTPHandler(a.metrics))
	}

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

// ReadyDetail is the readiness sweep with its detail intact: every declared
// check, whether it passed, its error and how long it took. [App.ReadyPath]
// answers with a bare verdict instead, because it sits on the public listener.
//
// Mount this behind your own authentication — it is the same data, and the point
// is that you choose who reads it:
//
//	admin.Get("/readyz", adaptor.HTTPHandler(a.ReadyDetail()))
//
// Returning the handler rather than the [health.Registry] is deliberate: the
// registry can also register checks, and those must go through [App.Declare] so
// the boot report and the sweep cannot disagree about what this process is.
func (a *App) ReadyDetail() http.Handler { return a.health.DetailHandler() }

// Context is the application's lifetime. Start background work from it: it is
// cancelled once the HTTP server has drained, not when shutdown begins, so a
// request accepted during the drain can still start work the drain waits for.
//
// A method rather than a field, per the standard library's own shape for a
// stored context (http.Request.Context).
func (a *App) Context() context.Context { return a.ctx }

// StreamContext returns the context a long-lived response should select on —
// SSE, a WebSocket, an NDJSON feed — cancelled before the HTTP server drains so
// the stream ends rather than holding the drain open for its full timeout.
// [App.Context] cannot serve here: it is cancelled after the drain.
//
// Derives from the request context, so the request's trace span carries into the
// stream. Fiber does not cancel that context on client disconnect — a failed
// write is still how you learn that.
//
// Call cancel from inside the stream body, not the handler: with
// SetBodyStreamWriter the handler returns first, so a cancel deferred there
// would sever the stream immediately. Skipping it leaks a drain registration.
//
//	ctx, cancel := h.app.StreamContext(c)
//	c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
//		defer cancel()
//		for {
//			select {
//			case ev := <-events:
//				// ...
//			case <-ctx.Done():
//				return
//			}
//		}
//	})
func (a *App) StreamContext(c fiber.Ctx) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(c.Context())
	// stop() matters: nothing cancels a Fiber request context, so an unreleased
	// registration accumulates once per stream ever served.
	stop := context.AfterFunc(a.drain.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (a *App) closeDraining() { a.drain.release() }
