# Laying out a service

neokit gives you the operational layer — boot, shutdown, errors, logs, login.
This page is about the part it deliberately does not own: where your own code
goes. It describes one layout, the one [`examples/reference`](../examples/reference)
is built in, and the reason behind each rule, so you can tell when a rule
applies and when it does not.

There is no generator. Copy `examples/reference`, change the module path, and
replace `tasks` with your own domain.

## The shape

```
cmd/api/
  main.go              config, exit code
  wiring.go            the composition root: the one file that names every package
internal/
  domain/              what your things are and what may happen to them
  app/<service>/       use cases, and the ports they declare for themselves
  adapter/
    sqlite/            one Store, implementing every port
    http/
      httpx/           what the HTTP modules share: the error mapping
      <module>http/    one package per module, each with one Register
```

Dependencies point inward, and only inward:

| Ring | Packages | May import |
| --- | --- | --- |
| Domain | `internal/domain` | the standard library, `neokit/errs` |
| App | `internal/app/*` | domain |
| Adapters | `internal/adapter/*` | app, domain, neokit, drivers |
| Composition root | `cmd/api` | everything |

The test of whether it holds: the domain compiles with the `adapter` directory
deleted, and a use case can be called from a handler, a scheduled job and a
command-line tool without any of them knowing about the other two.

## The rules, and why

**The domain takes its clock and its ids as arguments.** A type that calls
`time.Now` or mints its own id cannot be tested without them. `NewTask(id,
title, now)`.

**Shared failures are neokit's values, not copies.** `domain.ErrNotFound =
errs.ErrNotFound`. A copy compiles, reads the same, and never matches
`errors.Is` — every "not found" then leaves as a 500. Declare a sentinel of your
own only for a failure that is genuinely yours, and give it a case in
`httpx.MapError`.

**Each app service declares the storage it needs.** The interface lives next to
its consumer and is only as wide as the methods used. The store satisfies it
without naming it; a test passes a map. One line in the store's test file —
`var _ tasks.Repository = (*sqlite.Store)(nil)` — moves a mismatch from the
composition root to the package that caused it.

**App services know nothing about HTTP.** No status codes, no request types.
They return domain errors; the adapter decides what those look like on the
wire. A rule that needs fakes of two collaborators to test belongs here, not in
a handler.

**One store, one type.** `adapter/sqlite` is a single package with a single
`Store`. Group its methods into files by concern (`task.go`, `invoice.go`);
`store.go` owns the connection, the schema and shared helpers. A store type per
aggregate is a second abstraction over the same `*sql.DB`, and the narrow
interfaces in the app layer already keep each service from seeing more than it
uses.

**Migrations are append-only.** A step's position is its version. Editing one
that has shipped diverges from every database in the field; a fix is a new step
at the end.

**One HTTP package per module, one `Register` each.** `Register(api
fiber.Router, d Deps)` takes `a.API`, which is already mounted at the versioned
base, so a route never names `/api/v1`. Module packages never import each
other; what they share goes in `httpx`. Adding a module is adding a directory
and one call in `wiring.go`.

**A module is handed its guards and applies them itself.** `Deps.Member` and
`Deps.Owner` come from the login gate; the module decides which route takes
which. That knowledge belongs with the routes. A central list of protected
paths drifts from them, and a prefix match tends to catch a public route that
happens to share the prefix.

**Handlers return errors; they do not render them.** The app's error handler
turns a returned error into the envelope, consulting your `MapError` first and
neokit's standard sentinels second. No handler picks a status for a failure, so
no two handlers can pick different ones for the same failure.

**Dependencies arrive in a named struct.** `tasks.Deps`, `taskshttp.Deps`. The
call in the composition root then reads as a list of what that package can
reach, and adding a collaborator does not change every caller.

**The composition root pushes each resource as it builds it.**
`a.Shutdown.Push("database", …)` right after opening it. The stack unwinds in
reverse, behind the HTTP drain and the background work, so the order things
close in is the order they were built in, backwards — readable in one file, and
assertable with `a.Shutdown.Names()`.

**Background work runs on `a.Context()`.** Start a `jobs.Job` or `jobs.Daily`
with `.Start(a.Context())`, anything else with `safe.Go`. It is then stopped
and joined before your own teardown steps, with no goroutine of yours to
remember. Use `jobs.Daily` for anything daily: `Every: 24 * time.Hour` drifts an
hour at each daylight-saving change and never drifts back.

### What not to do

- Don't split `domain`, `app/<service>` or the store into sub-packages before
  something forces it. Files group; packages are for boundaries.
- Don't branch on an integration's name in the app layer (`switch provider {
  case "stripe": … }`). That decision belongs behind an interface the
  integration implements.
- Don't put a route's permission anywhere but on the route.

## API conventions

These are what `fiberx` assumes, and what a client written against one neokit
service can rely on in the next.

| | |
| --- | --- |
| Errors | `{"error": "<public sentence>", "code": "<stable slug>", "fields": […]?}`. The cause is logged, never sent. `fields` only for a validation failure. |
| Created | `201` with the resource and a `Location` — `fiberx.Created(c, fiberx.MemberURL(c, id), body)`. |
| Commands, deletes | `204 No Content`. Never `{"ok": true}`. |
| Parameters | Path segments identify a resource, the query filters, the body carries what a write changes. `POST /charge-limit` + `{"percent": 80}`, not `/charge-limit/80`. |
| Lists | A bare array, never `null`. The moment one needs paging or metadata, that endpoint becomes `{"items": […], …}`. |
| Concurrent edits | `ETag` on reads, opt-in `If-Match` on writes, `412` when it no longer matches — `fiberx.SetVersionETag`, `fiberx.IfMatchVersion`. |
| Instants | RFC 3339, UTC, whole seconds. |

## What is already defended, and what is yours

`app.New` turns these on without being asked. Each is adjustable through
`app.Options`; none needs remembering.

| Defence | Default | Adjust with |
| --- | --- | --- |
| Response headers | `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, a CSP that allows nothing | `Options.Security` → `Headers` |
| Cross-site writes | A browser's write from another origin is refused (`403 cross_origin`) unless the origin is in `CORS_ORIGINS` | `Options.Security` → `TrustedOrigins`, `AllowCrossOriginWrites` |
| Request bounds | 1 MiB body, 16 KB headers, 15 s to send a request, 120 s idle | `Options.Fiber` |
| Error bodies | Public sentence and code only; panics recovered and logged with a stack | `a.Errors.Mapper` |
| Client address | The connection's own peer; forwarded headers ignored | `TRUSTED_PROXIES` |
| Metrics | Route templates only, never raw paths; open unless `METRICS_TOKEN` is set | `METRICS_TOKEN` |
| Outbound calls | `httpc` clients are bounded, retried, and can refuse internal addresses | `httpc.GuardedTransport` |

The cross-site check is the one to understand rather than just leave on. It
matters most for a service with *no* login, open on a private network: any page
its users visit can make their browser send it a request, and that check is the
only thing in the way. See `fiberx.CrossOriginGuard` for the exact rule.

These are decisions neokit cannot make for you:

- **Whether there is a login.** `fiberauth` is off until an identity provider is
  configured, and its guards pass everything through while it is off. A service
  that must never run open should refuse to start without one.
- **Which routes need what.** `gate.RequireAuth()` and `gate.RequireOwner()` are
  the two guards. Anything finer — per-record ownership, roles — is a check in
  your app layer against `fiberauth.IdentityFrom`.
- **What a token may not do.** A personal API token does what its maker may. Put
  `fiberauth.RefuseAPIToken` ahead of the guard on any route that could mint
  credentials, change who may sign in, or hand over a stored secret.
- **Rate limits.** `fiberx.RateLimiter` and `RateLimiterByPeer` are per route;
  only you know which routes are expensive. Behind a proxy, set
  `TRUSTED_PROXIES` first, or every caller shares the proxy's budget.
- **HSTS.** Off by default because it outlives a mistake by its whole max-age.
  Turn it on in `Options.Security` once the hostname is HTTPS for good.
- **TLS, and secrets at rest.** neokit terminates no TLS and encrypts no column.
  Put a proxy in front for the first, and treat the database file and its
  backups as holding whatever you store in them.

## Testing each ring

| Ring | Test against | Why |
| --- | --- | --- |
| Domain | nothing | It has no dependencies to fake. |
| App | a map-backed fake of its own port | The rule is under test, not SQL. |
| Store | a real database file under `t.TempDir()` | A fake of SQL proves the fake agrees with itself. Not `:memory:` — it is private per pooled connection. |
| Wiring | `wire(cfg)` and `a.HTTP.Test` | The real chain and the real store, with nothing listening on a port. |

After writing a test for a rule that matters, break the rule and watch the test
fail. A test that passes either way is documentation, not a check.

## Starting a new service

1. Copy `examples/reference`; replace the module path in every import.
2. Rename the app (`Name:` in `wiring.go`), the cookie prefix and the database
   path default. The cookie prefix is also the API-token prefix; choose it once.
3. Replace `domain/task.go` with your first aggregate and its rules.
4. Write the use cases in `app/<service>/`, with the `Repository` they need.
5. Add a migration step and the store methods; keep the `var _` assertion.
6. Write the module's `Register` and handlers; add your sentinels to
   `httpx.MapError`.
7. Wire it in `wiring.go`, pushing each resource as it is built.
8. Set `CORS_ORIGINS` to your frontend's origin, `TRUSTED_PROXIES` to your
   proxy's address, and the four `OIDC_*` variables when it should stop being
   open.
