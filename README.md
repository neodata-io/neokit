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
by string: every dependency `app` builds is an exported field on it — including
`a.API`, a route group already mounted at `a.APIBase`, so route registration
never concatenates a version prefix by hand — and a handler stays an ordinary
Fiber handler over ordinary types. Reflection is
confined to the two places Go offers no alternative — decoding the environment
in `config`, and `validator` tags in `fiberx`. Nothing reaches a binary unless
its package is imported.

Two globals:

- `app.New` installs its logger as `slog.Default()`, unless you pass your own
  through `Options.Log`.
- `safe.Go` and the `jobs` `Start` methods run on `safe`'s package-level group, which
  `app.Run` drains as its `background-work` shutdown step. A subsystem that wants
  its own drain uses a `safe.Group` of its own.

Pre-1.0: the API may change between `v0.x` releases.

## A new project

There is no generator. A service starts as one file, and each feature is one
call — calling it is enabling it:

```go
cfg, err := config.Load[config.Base](".env")   // .env only fills gaps, and only when named
a, err := neokit.New(app.Options{Name: "okstables", Base: cfg})
defer a.Close()

db, err := a.Database(cfg.DatabasePath, nil)   // ✓ database + ordered shutdown

a.API.Get("/hello", func(c fiber.Ctx) error {  // ✓ mounted at /api/v1
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
trace ids, the `{"error": …}` envelope, panic recovery, security headers, a
refusal for writes a browser sends from another site, CORS, compression,
request/idle timeouts, `/healthz`, `/metrics`, OTLP export when you point it
somewhere, and an ordered SIGTERM drain. You did not configure any of it and you
can override all of it. Readiness is the one thing `app` does not decide for
you — see below.

A service with more than one file has a shape worth getting right once.
[`examples/reference`](examples/reference) is a complete small service in the
layout neokit is meant for — domain, use cases, one store, one HTTP module, a
composition root — and [`docs/architecture.md`](docs/architecture.md) explains
what goes where and why. Copy the directory to start.

Even the version fills itself in — `go build` embeds the commit, so every log
line says `okstables dev (a1b2c3d, dirty)` without a Makefile.

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

**Without `METRICS_TOKEN` the endpoint answers only direct scrapes**, which is
fine on a private network; a request forwarded by a proxy in `TRUSTED_PROXIES`
gets 403. Set `METRICS_TOKEN` and it requires `Authorization: Bearer …` — the
same header Prometheus sends from `authorization:` in a scrape config —
wherever the scrape comes from.

`/healthz` is mounted for you and answers 200 unconditionally — a liveness probe
that consulted a dependency would have Kubernetes restart a healthy process
because its database blinked. Probes and scrapes are registered above the
middleware chain, so none of them are logged or counted: a ten-second liveness
interval is 8 640 log lines a day, and metering a scrape means recording a data
point about the act of collecting data points.

**Readiness is yours to mount**, because only you know what "ready" means for
this service and where the answer may safely be read. `health` is the registry:

```go
ready := &health.Registry{Log: a.Log}
ready.Register("database", func(ctx context.Context) error { return db.PingContext(ctx) })

a.HTTP.Get("/readyz", adaptor.HTTPHandler(ready.ReadyHandler()))     // {"ready":false}
admin.Get("/readyz", adaptor.HTTPHandler(ready.DetailHandler()))     // the full sweep
```

`ReadyHandler` answers with the verdict and nothing else, because the audience on
a public port is an orchestrator that reads only the status code, and naming your
dependencies there would give away a map of your infrastructure to buy nothing.
The detail is not lost:

- it goes to the **log**, once per transition rather than once per probe:
  `WARN not ready failing="database: connection refused"`, then `INFO ready`;
- `DetailHandler` is the same sweep in full, for a route behind your own
  authentication:

```json
{"ready":false,"checks":[{"name":"database","ok":false,
                          "error":"connection refused","tookMs":3}]}
```

Shutdown is the same shape — a name and a function, in one place:

```go
a.Shutdown.Push("plugins", func(context.Context) error { return manager.Close() })
```

The stack unwinds LIFO after the HTTP drain, so a dependency closes after the
requests still using it have finished.

## Safe before you configure anything

The defaults are chosen for the service written in a hurry, because that is the
one that ships without them.

- **Response headers** on every answer, errors included: `nosniff`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and a
  Content-Security-Policy that lets a browser render nothing.
- **Writes from another site are refused.** A browser labels each request with
  where it came from; a state-changing one from another origin answers
  `403 cross_origin` unless that origin is in `CORS_ORIGINS`. No token, no
  state — the rule is Go's own `net/http.CrossOriginProtection`. This is what
  protects a service with *no* login on a private network, which any page its
  users visit can otherwise reach through their browser.
- **Bounded requests**: 1 MiB bodies, 16 KB of headers, a deadline for sending
  one.
- **Errors say nothing they should not**: a public sentence and a stable code;
  the cause goes to the log.
- **No header is believed** about who the client is until you name the proxy
  that wrote it: `TRUSTED_PROXIES=10.0.0.2` (or a CIDR range). The client
  address then comes from `X-Forwarded-For`, read from the right, past the
  proxies named and no further — and a mistyped entry fails at boot.

All of it is one hook away, in the same shape as the Fiber one:

```go
a, err := app.New(app.Options{
    Name: "myapp", Base: cfg.Base,
    Security: func(s *app.Security) {
        s.Headers.HSTS = 365 * 24 * time.Hour          // once the host is HTTPS for good
        s.Headers.ContentSecurityPolicy = "default-src 'self'" // if you also serve pages
    },
})
```

Together they cost about 150 ns and no allocation per request — measured by
`BenchmarkSecurityHeaders` and `BenchmarkCrossOriginGuard_Write` in `fiberx`.

## Login gate in ten lines

```go
authn, ok := oidcauth.New(oidcauth.Config{
    Issuer:   os.Getenv("OIDC_ISSUER"),   ClientID:     os.Getenv("OIDC_CLIENT_ID"),
    BaseURL:  os.Getenv("OIDC_BASE_URL"), ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
})
sessions, err := session.NewSQLite(db)   // creates its own table; or bring your own store
gate := a.Login(fiberauth.Options{
    Provider:     func() *oidcauth.Provider { if !ok { return nil }; return authn },
    Sessions:     sessions,
    CookiePrefix: "myapp",
})
admin := a.API.Group("/admin", gate.RequireOwner())
```

`Login` mounts the identity middleware, then the handshake routes — in that
order, which is the one a caller cannot arrange by hand: you need the gate to
obtain the middleware, and by then the routes would already be mounted.

`ok == false` (no credentials configured) means the gate is off: the middleware
returns immediately, the guards pass through, and the handshake routes 404 — so
an app can ship open and close later without a second feature flag. A service
that must never run open sets `Required: true`: with no login configured its
guards answer 503 instead of passing everyone through.

The expired-session sweep comes with it: a store that can prune in one statement
is swept daily for as long as the app runs. Building the gate directly with
`fiberauth.New(a.App, …)` skips only that step — start it yourself with
`safe.Go(a.Context(), "session sweep", func() { gate.Run(a.Context()) })`.

## API tokens for scripts and apps

A browser signs in; a script, a CI job or a native app presents a token. One
more field turns them on:

```go
tokens, err := apitoken.NewSQLite(db, apitoken.SQLiteOptions{})
gate := a.Login(fiberauth.Options{
    Provider: provider, Sessions: sessions, CookiePrefix: "myapp",
    Tokens:      tokens,
    TokenMaxAge: 90 * 24 * time.Hour, // optional: a year when unset, negative for no limit
})
```

`Authorization: Bearer myapp_…` is then recognised wherever a session is, so
your guards and handlers do not know the difference, and the gate mounts the
routes that manage tokens:

| Route | Who | |
| --- | --- | --- |
| `GET`, `POST {api}/auth/me/tokens` | signed in, by browser | list your own, or make one — shown this once |
| `DELETE {api}/auth/me/tokens/:id` | signed in, by browser | revoke one of yours |
| `DELETE {api}/auth/me/token` | the token itself | how an app signs out |
| `GET {api}/auth/tokens`, `DELETE …/:id` | owner, by browser | see and revoke everyone's |

A token is 256 random bits behind your prefix; only its SHA-256 is stored. It
acts as its maker, with ownership re-derived on every request, and belongs to
the login it was made under — point the service at another identity provider
and every token is retired by that alone. A token that cannot be matched
answers 401 even on an open route, rather than carrying on as nobody.

What a token may not do is the part to extend. The routes above refuse one
(`403 token_not_allowed`), so a leaked token cannot mint another. Put
`fiberauth.RefuseAPIToken` ahead of the guard on any route of your own that
could mint credentials, change who may sign in, or hand over a stored secret:

```go
admin.Put("/login", fiberauth.RefuseAPIToken, gate.RequireOwner(), h.changeLogin)
```

Already have token routes — paths your clients call, or a sign-in flow that
ends in a token? Keep them, leave `Tokens` unset, and take the recognition
alone. It is the same code the gate mounts, so the 401, the expiry check and the
per-request owner are not yours to get right twice:

```go
gate := a.Login(fiberauth.Options{Provider: provider, Sessions: sessions})
a.HTTP.Use(fiberauth.APITokenMiddleware(provider, tokens, "myapp_")) // after the gate, ahead of your routes
```

## Runnable examples

| Need | Example |
| --- | --- |
| A small HTTP service | [`examples/minimal-api`](examples/minimal-api) |
| HTTP, health, metrics, shutdown, and SQLite | [`examples/batteries`](examples/batteries) |
| A bounded, retrying external API client | [`examples/external-client`](examples/external-client) |
| A whole service in layers: domain, use cases, store, HTTP module, login, tokens, a nightly job — with tests for each | [`examples/reference`](examples/reference) |

Run one from the repository root with `go run ./examples/minimal-api`. They are
ordinary Go types and constructors: copy the pieces your service needs rather
than adopting the whole thing.

## Packages

`app` is one way in, not the only one. Every package below stands alone, so a
service that wants a retrying HTTP client and nothing else imports `httpc` and
gets exactly that.

| Area | Package | What |
| --- | --- | --- |
| Boot and shutdown | `app` | the builder: config, logger, middleware chain, probes, `/metrics`, `Run` with an ordered drain |
| | `neokit` | `neokit.New` — `app` plus one method per feature: `a.Database`, `a.Login`, `a.Backups`, the notify senders |
| | `lifecycle` | `Signals` context + a LIFO named shutdown `Stack` |
| | `config` | `config.Base`, the settings every service has, and `Load` to parse the environment into your own struct |
| | `buildinfo` | version/commit/date with Go's embedded VCS metadata as the fallback |
| HTTP in | `fiberx` | `{"error": …}` envelope, bind+validate, metrics/logging middleware, security headers, the cross-origin write guard, a rate limiter, `Created` + `Location`, strong ETags + 304 for cacheable bytes (`NotModified` before the work) |
| Login | `oidcauth` | provider-agnostic OpenID Connect relying party: PKCE, nonce, typed error sentinels |
| | `oidcauth/fiberauth` | the browser half — handshake routes, `__Host-` cookies, session middleware, guards — and API tokens recognised wherever a session is |
| | `session` | `Session`, `Store`, `Policy` — and `NewSQLite`, the store neokit ships. Standard library only |
| | `apitoken` | `Token`, `Store` — and `NewSQLite`: personal API tokens, stored as hashes, with optional expiry. Standard library only |
| HTTP out | `httpc` | `NewHTTPClient`, `BaseClient`, retry transport, `APIError`, `Classify` + `Fault.LogLevel`, SSRF guard |
| | `notify` | `Sender` — webhook, ntfy, Apprise, and a `Multi` fan-out |
| | `webpush` | VAPID keypair generation and `sub` normalisation (no dependencies) |
| | `webpush/delivery` | `Sender` — delivers to every subscription on a topic and prunes the ones that have gone away |
| Data | `sqlitex` | `PRAGMA user_version` append-only migration runner, `Query`/`Each`/`QueryOne`, `InTx`, `Placeholders`/`Args`/`CheckAffected`, `VACUUM INTO` snapshot backup |
| | `backup` | a fresh snapshot streamed on demand, and a dated copy written daily at a set local time, with retention |
| | `cache` | stale-while-revalidate `GetOrFetch`, `GetOrFetchWithBackoff` for a source that may be down from the start; `Fetch` adds a `Status` saying when a value is only being kept alive through an outage |
| Background | `safe` | goroutine recovery, `Group` supervision, `Pool` for concurrency-capped one-shot tasks, `All`/`AllLimit` fan-out |
| | `jobs` | `Job{Every, Timeout, RunAtStart, Do}` — periodic work that is bounded and panic-guarded |
| | `pubsub` | `Bus[T]` — keyed fan-out for SSE/WebSocket, drop-on-slow |
| Observability | `logx` | `slog` context handler, canonical `Err` attribute, request-id propagation |
| | `tracing` `metrics` | OpenTelemetry traces and metrics — `/metrics` to scrape, OTLP to push, one set of instruments |
| | `health` | liveness/readiness registry — bounded sweep, terse public verdict, detail behind your own auth |
| Small | `errs` | `ErrNotFound`, `ErrInvalidInput`, `ErrConflict` — sentinels `fiberx` renders without a mapper |
| | `ids` `clock` | id/token generation, and a real and a fake `Now()` |
| | `capset` | generic capability resolution by type assertion, memoised |
| | `disk` | `Usage` — free/total filesystem space via `syscall.Statfs` |
| | `netx` | `AddrInUseHint` — a readable message for a listener already bound |

Every package is independent: importing one never drags in another's dependencies.
A binary that does not import `oidcauth` links neither `go-oidc` nor `oauth2`.

MIT.
