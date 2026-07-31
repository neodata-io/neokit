# neokit

Composable Go building blocks for small services. Neokit does not replace the
standard library or a web framework; it is the operational layer around them —
safe HTTP clients, structured errors and logs, health checks, ordered shutdown,
observability, caching, and optional integrations.

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

## Packages

| Package | What |
| --- | --- |
| `logx` | `slog` context handler, canonical `Err` attribute, request-id propagation |
| `httpc` | `NewHTTPClient`, `BaseClient`, retry transport, `APIError`, `Classify`, SSRF guard |
| `fiberx` | `{"error": …}` envelope, bind+validate, metrics/logging middleware, rate limiters |
| `cache` | stale-while-revalidate `GetOrFetch` |
| `sqlitex` | `PRAGMA user_version` append-only migration runner, `VACUUM INTO` snapshot backup |
| `tracing` `metrics` | OpenTelemetry and Prometheus wiring |
| `debugserver` | `/metrics` and opt-in pprof on a listener of their own, off the application's port |
| `safe` `ids` `clock` | goroutine recovery, id/token generation, injectable clock |
| `netx` | `AddrInUseHint` — a readable message for a listener already bound |
| `disk` | `Usage` — free/total filesystem space via `syscall.Statfs` |
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

## Start here

| Need | Example |
| --- | --- |
| A small HTTP service | [`examples/minimal-api`](examples/minimal-api) |
| HTTP, health, metrics, shutdown, and SQLite | [`examples/production-service`](examples/production-service) |
| A bounded, retrying external API client | [`examples/external-client`](examples/external-client) |

Run one from the repository root with `go run ./examples/minimal-api`. They are
ordinary Go types and constructors: copy the pieces your service needs rather
than adopting the whole thing.

## A new project

There is no generator. A service starts as one file — copy the shape from
`app`'s package documentation, embed `config.Base` in your own config struct,
and add what you need:

```go
cfg, err := config.Load[Config]()
a, err := app.New(app.Options{Name: "okstables", Version: version, Base: cfg.Base})
defer a.Close()

a.Fiber.Get("/api/v1/hello", func(c fiber.Ctx) error {
    return c.JSON(fiber.Map{"hello": "okstables"})
})
return a.Run()
```

That is the whole boot. A scaffolding command would only write this out for you
and then need a template set kept in step with the API forever.

## The diagnostics port

`app` serves Prometheus metrics, `/healthz` and `/readyz` on a second listener,
bound to `127.0.0.1:9090`. Loopback is the default because a readiness body
names your dependencies and their errors — operational detail, not public API.
**A scrape from another container needs `METRICS_BIND_ADDR=0.0.0.0`.** pprof is
off unless `ENABLE_PPROF=true`, because a heap profile carries whatever secrets
are live in the process.

Neither fact is one you should have to infer from a default, so the boot report
states both before the listeners come up:

```text
production-service 1.4.0 · :8080
  ✓ database        ./data/app.db
  ✓ diagnostics     127.0.0.1:9090 · metrics, health
  ✗ metrics export  OTEL_EXPORTER_OTLP_ENDPOINT unset
  ✗ tracing         OTEL_EXPORTER_OTLP_ENDPOINT unset
```

That block is generated from the same `app.Subsystem` declarations that register
the `/readyz` checks, so it cannot drift from what the process actually is.

## Login gate in ten lines

```go
authn, ok := oidcauth.New(oidcauth.Config{
    Issuer:   os.Getenv("OIDC_ISSUER"),   ClientID:     os.Getenv("OIDC_CLIENT_ID"),
    BaseURL:  os.Getenv("OIDC_BASE_URL"), ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
})
gate := fiberauth.New(fiberauth.Options{
    Provider:     func() *oidcauth.Provider { if !ok { return nil }; return authn },
    Sessions:     store,      // your own storage; neokit ships none
    CookiePrefix: "myapp",
})
app.Use(gate.ResolveIdentity())
gate.Register(app)
admin := app.Group("/api/v1/admin", gate.RequireOwner())
```

`ok == false` (no credentials configured) means the gate is off: the middleware
returns immediately, the guards pass through, and the handshake routes 404 — so
an app can ship open and close later without a second feature flag.

MIT.
