# neokit

Composable Go building blocks for small services. Neokit does not replace the
standard library or a web framework; it is the operational layer around them —
safe HTTP clients, structured errors and logs, health checks, ordered shutdown,
observability, caching, and optional integrations.

neokit builds on [Fiber v3](https://github.com/gofiber/fiber). `app.HTTP` is a
`*fiber.App` and handlers are ordinary Fiber handlers — a deliberate choice, not
an implementation detail waiting to be abstracted away.

Take one package at a time, or start with `app` when a service wants the whole
boot sequence from one constructor. There is no service container and no lookup
by string: every dependency `app` builds is an exported field on it, and a
handler stays an ordinary Fiber handler over ordinary types. Reflection is
confined to the two places Go offers no alternative — decoding the environment
in `config`, and `validator` tags in `fiberx`. Nothing reaches a binary unless
its package is imported.

The one global: `app.New` installs its logger as `slog.Default()`, unless you
pass your own through `Options.Log`.

Pre-1.0: the API may change between `v0.x` releases.

## A new project

There is no generator. A service starts as one file, and each feature is one
call — calling it is enabling it:

```go
cfg, err := config.Load[config.Base]()
a, err := neokit.New(app.Options{Name: "okstables", Base: cfg})
defer a.Close()

db, err := a.Database(cfg.DatabasePath, nil)   // ✓ database  + /readyz + shutdown

a.HTTP.Get("/api/v1/hello", func(c fiber.Ctx) error {
    return c.JSON(fiber.Map{"hello": "okstables"})
})
return a.Run()
```

`neokit.New` is the batteries-included layer — `a.Database`, `a.Login`,
`a.Backups`, `a.Ntfy` — and importing it compiles every feature it fronts. A
service that wants less imports `neokit/app` and the feature packages it
actually uses; they compose identically.

That is the whole boot, and it is already a production shape. `Load`, `New`,
`Close`, `Run` — four calls — get you structured logging, a request log with
trace ids, the `{"error": …}` envelope, panic recovery, CORS, compression,
request/idle timeouts, `/healthz`, `/readyz`, `/metrics`, OTLP export when you
point it somewhere, an ordered SIGTERM drain, and a boot report describing all of
it. You did not configure any of it and you can override all of it.

Even the version fills itself in — `go build` embeds the commit, so the report
and every log line say `okstables dev (a1b2c3d, dirty)` without a Makefile.

Add your own settings by embedding `config.Base` in a struct of your own, and
your own version once you have release builds to stamp:

```go
type Config struct {
    config.Base
    Issuer string `env:"OIDC_ISSUER"`
}

a, err := app.New(app.Options{
    Name:    "okstables",
    Version: buildinfo.Get(version, commit, date).String(),
    Base:    cfg.Base,
})
```

A scaffolding command would only write this out for you and then need a template
set kept in step with the API forever.

## Metrics and probes

One set of instruments, two ways out, no wiring. Every instrument in the process
— the HTTP server histogram, Go runtime stats, whatever you declare yourself —
is OpenTelemetry-native and leaves by both exits at once:

- **`GET /metrics`**, Prometheus text format, on the application listener.
  Always mounted, nothing to configure. `http.server.request.duration` arrives as
  `http_server_request_duration_seconds` with your route templates intact.
- **OTLP push** to `OTEL_EXPORTER_OTLP_ENDPOINT`, when you set one. Traces and
  metrics share the gate; the histogram carries trace exemplars, so a slow bucket
  in Grafana links straight to its span in Tempo.

Nothing is declared or recorded twice — the two are readers on one MeterProvider,
so a metric you add anywhere shows up on both without being touched again.

**The endpoint is unauthenticated by default**, which is what Grafana and Traefik
do and is fine on a private network. Set `METRICS_TOKEN` and it requires
`Authorization: Bearer …` — the same header Prometheus sends from `authorization:`
in a scrape config. The boot report says which of the two you are running, because
nothing else will.

`/healthz` and `/readyz` are on the same listener. Probes and scrapes are all
registered above the middleware chain, so none of them are logged or counted: a
ten-second liveness interval is 8 640 log lines a day, and metering a scrape means
recording a data point about the act of collecting data points.

Readiness answers with the verdict and nothing else — `{"ready":false}` — because
the audience on a public port is an orchestrator that reads only the status code,
and naming your dependencies there would give away a map of your infrastructure
to buy nothing. The detail is not lost:

- it goes to the **log**, once per transition rather than once per probe:
  `WARN not ready failing="database: connection refused"`, then `INFO ready`;
- `app.ReadyDetail()` is the same sweep in full, for a route you mount behind
  your own authentication:

```go
admin.Get("/readyz", adaptor.HTTPHandler(a.ReadyDetail()))
// {"ready":false,"checks":[{"name":"database","ok":false,
//                           "error":"connection refused","tookMs":3}]}
```

The report states all of it before the listener comes up:

```text
production-service 1.4.0 · :8080
  ✓ database          ./data/app.db
  ✓ health            /healthz, /readyz
  ✓ metrics endpoint  /metrics · unauthenticated · set METRICS_TOKEN to require a bearer token
  ✗ metrics export    OTEL_EXPORTER_OTLP_ENDPOINT unset
  ✗ tracing           OTEL_EXPORTER_OTLP_ENDPOINT unset
```

That block, the `/readyz` checks and the shutdown steps all come from one
registration, so they cannot drift from what the process actually is. neokit's
own features register themselves — `a.Database` is the database line, the ping
and the close. For a dependency you built, say what you need by name:

```go
a.ClosesOnShutdown("plugins", "3 loaded", manager.Close)   // report line + teardown
a.ChecksReadiness("cache", addr, client.Ping)              // report line + /readyz
```

Named once, so the report, `/readyz` and the shutdown log cannot call one thing
by two names.

## Login gate in ten lines

```go
authn, ok := oidcauth.New(oidcauth.Config{
    Issuer:   os.Getenv("OIDC_ISSUER"),   ClientID:     os.Getenv("OIDC_CLIENT_ID"),
    BaseURL:  os.Getenv("OIDC_BASE_URL"), ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
})
sessions, err := session.NewSQLite(db)   // creates its own table; or bring your own store
gate := fiberauth.New(a, fiberauth.Options{
    Provider:     func() *oidcauth.Provider { if !ok { return nil }; return authn },
    Sessions:     sessions,
    CookiePrefix: "myapp",
})
admin := a.HTTP.Group("/api/v1/admin", gate.RequireOwner())
```

`New` mounts the identity middleware, then the handshake routes, then adds the
`login` line to the boot report — in that order, which is the one a caller cannot
arrange by hand: you need the gate to obtain the middleware, and by then the
routes would already be mounted.

`ok == false` (no credentials configured) means the gate is off: the middleware
returns immediately, the guards pass through, and the handshake routes 404 — so
an app can ship open and close later without a second feature flag.

The expired-session sweep comes with it. A store that can prune in one statement
is pruned daily, joined at shutdown, and named on the same `login` line — nothing
to start and nothing to remember.

## Runnable examples

| Need | Example |
| --- | --- |
| A small HTTP service | [`examples/minimal-api`](examples/minimal-api) |
| HTTP, health, metrics, shutdown, and SQLite | [`examples/production-service`](examples/production-service) |
| A bounded, retrying external API client | [`examples/external-client`](examples/external-client) |

Run one from the repository root with `go run ./examples/minimal-api`. They are
ordinary Go types and constructors: copy the pieces your service needs rather
than adopting the whole thing.

## Packages

`app` is one way in, not the only one. Every package below stands alone, so a
service that wants a retrying HTTP client and nothing else imports `httpc` and
gets exactly that.

| Package | What |
| --- | --- |
| `logx` | `slog` context handler, canonical `Err` attribute, request-id propagation |
| `httpc` | `NewHTTPClient`, `BaseClient`, retry transport, `APIError`, `Classify`, SSRF guard |
| `fiberx` | `{"error": …}` envelope, bind+validate, metrics/logging middleware, rate limiters |
| `cache` | stale-while-revalidate `GetOrFetch` |
| `sqlitex` | `PRAGMA user_version` append-only migration runner, `VACUUM INTO` snapshot backup |
| `tracing` `metrics` | OpenTelemetry traces and metrics — `/metrics` to scrape, OTLP to push, one set of instruments |
| `health` | liveness/readiness registry — bounded sweep, terse public verdict, detail behind your own auth |
| `safe` `ids` `clock` | goroutine recovery, id/token generation, injectable clock |
| `netx` | `AddrInUseHint` — a readable message for a listener already bound |
| `disk` | `Usage` — free/total filesystem space via `syscall.Statfs` |
| `session` | `Session`, `Store`, `Policy` — and `NewSQLite`, the store neokit ships. Standard library only |
| `oidcauth` | provider-agnostic OpenID Connect relying party: PKCE, nonce, typed error sentinels |
| `oidcauth/fiberauth` | the browser half — handshake routes, `__Host-` cookies, session middleware, guards |
| `jobs` | `Job{Every, Timeout, RunAtStart, Do}` — periodic work that is bounded and panic-guarded |
| `lifecycle` | `Signals` context + a LIFO named shutdown `Stack` |
| `pubsub` | `Bus[T]` — keyed fan-out for SSE/WebSocket, drop-on-slow |
| `capset` | generic capability resolution by type assertion, memoised |
| `notify` | `Sender` — webhook, ntfy, Apprise, and a `Multi` fan-out |
| `webpush` | VAPID keypair generation and `sub` normalisation (no dependencies) |
| `buildinfo` | version/commit/date with Go's embedded VCS metadata as the fallback |

Every package is independent: importing one never drags in another's dependencies.
A binary that does not import `oidcauth` links neither `go-oidc` nor `oauth2`.

MIT.
