# neokit

Go building blocks for small backend services: boot, shutdown, errors, logs,
login, SQLite, outbound HTTP. It is a library — other services import it
(NeoGate is the main consumer), so every exported name is a commitment and
every default ships to all of them.

Module `github.com/neodata-io/neokit`, Go 1.26+, HTTP on Fiber v3 (on purpose —
do not abstract it away). Pre-1.0: the API may change between `v0.x.0` tags.

## Commands

```sh
go build ./... && go vet ./...
go test -race ./...                              # what CI runs
go test -race ./httpc/ -run TestName             # one test
golangci-lint run ./...                          # v2, config in .golangci.yml
test -z "$(gofmt -l .)"
test -z "$(go fix -diff ./...)"                  # CI fails if a modernizer has work left
go test ./fiberx/ -run xxx -fuzz FuzzIfMatchVersion -fuzztime 20s
go run ./examples/minimal-api
docker build -f examples/reference/Dockerfile -t reference .   # CI builds this too
```

`govulncheck` runs in CI only. All of the above must pass before work is called
done; say so plainly if one was not run.

## Map

| Area | Packages |
| --- | --- |
| Boot and shutdown | `app` (builder, middleware chain, `Run`), `lifecycle`, `config`, `buildinfo`, root `neokit` (batteries-included wrapper) |
| HTTP in | `fiberx` (error envelope, validation, security headers, cross-origin guard, rate limit, ETags) |
| Login | `oidcauth` (protocol), `oidcauth/fiberauth` (routes, cookies, guards, API tokens), `session`, `apitoken` |
| HTTP out | `httpc` (clients, retry, SSRF guard), `notify`, `webpush`, `webpush/delivery` |
| Data | `sqlitex` (open, migrate, query, snapshot), `backup`, `cache` |
| Background | `safe` (recovered goroutines, groups, pools), `jobs`, `pubsub` |
| Observability | `logx`, `tracing`, `metrics`, `health` |
| Small | `errs`, `ids`, `clock`, `capset`, `disk`, `netx` |

`docs/architecture.md` describes the service layout neokit is meant for;
`examples/reference` is that layout as running code with tests per layer.

## Design rules

- **Packages stand alone.** Importing one must not drag in another's
  dependencies. `session`, `apitoken` and `webpush` are standard library only.
  Only `app`, `oidcauth/fiberauth` and the root package may import widely. Check
  with `go list -f '{{.Imports}}' ./pkg` before adding an import.
- **No new module dependency** without asking first.
- **No container, no lookup by string, no reflection** outside `config` (env
  decoding) and `fiberx` (validator tags).
- **Two globals only:** `slog.Default()` set by `app.New`, and `safe`'s
  package-level group drained by `app.Run`. Do not add a third.
- **Options are structs with zero-value defaults**; a hook is a function
  (`Options.Fiber func(*fiber.Config)`), never a struct to merge.
- **Exported API grows last.** Fix with unexported code where possible. Before
  adding an exported name, state the problem and why nothing existing solves
  it. When touching a package, review its whole exported surface and report
  what is wrong, unprompted.

## Security invariants

These hold everywhere; a change that breaks one is wrong even if tests pass.

- **Fail closed.** Unknown, unparseable or unreachable means "no", not "yes".
  The one deliberate exception is the open login gate, and
  `fiberauth.Options.Required` turns that off.
- **Nothing secret leaves.** Clients get a public sentence and a stable code;
  the cause goes to the log. Never log or return a token, a secret, or a URL
  with credentials or a secret path — name the host. An upstream response body
  never reaches a client, and reaches a log only bounded; an identity
  provider's body is dropped entirely.
- **Tokens:** `crypto/rand`, 32 bytes, only the SHA-256 stored. Static secrets
  are compared in constant time.
- **Client address is `c.IP()` only.** Never read `X-Forwarded-For` yourself;
  `TRUSTED_PROXIES` is how a deployment vouches for it.
- **Labels are bounded.** Metrics, spans and the request log use the route
  template, never the raw path, and never a user-supplied value.
- **Caller-supplied URLs** go through `httpc.GuardedTransport`, which checks the
  resolved address at dial time. A check before the dial is not enough.
- **Browser cookies** are `__Host-` prefixed on HTTPS, `HttpOnly`, `SameSite=Lax`;
  read only the name that would have been written.
- **Files:** new directories `0700`, new database and backup files `0600`.
  Write to a temp name and rename; delete only files this code created.

## Fiber traps

- Strings from `c.Path()`, `c.Get()`, `c.Params()` alias a pooled buffer. Copy
  one (`strings.Clone`) before it outlives the handler: a goroutine, a span, a
  cache, a struct that is stored.
- In middleware, `c.Route()` is only the matched route **after** `c.Next()`.
- Middleware applies to routes registered after it. Order in `app.newFiber` is
  deliberate and pinned by tests.
- A returned error is rendered after middleware unwinds; read the status with
  `Errors.StatusForError`, not `c.Response().StatusCode()`.

## SQLite

- `sqlitex.Open` sets WAL, `busy_timeout`, foreign keys and immediate
  transactions in the DSN so every pooled connection has them. Pass
  `sql.TxOptions{ReadOnly: true}` for a read-only transaction.
- Migrations are append-only: a step's position is its version. Never edit one
  that has shipped.
- Stores that own a table (`session`, `apitoken`) create it with
  `IF NOT EXISTS` and add later columns themselves, so an existing table is
  adopted without a migration.

## Tests

- Test first. Write the test, run it, see it fail for the right reason, then
  write the fix. A bug fix without a test that failed first is not done.
- Names are sentences about behaviour: `TestExpiredTokensDoNotCountTowardTheLimit`.
- Real things over fakes: a database file under `t.TempDir()` (never
  `:memory:` — it is private per pooled connection), `httptest` servers,
  `app.HTTP.Test`. Use `t.Context()`.
- A security rule gets a test that states the rule, and parsers of untrusted
  input get a fuzz test with the property they must keep.
- `tracing`, `metrics` and anything concurrent must be exercised under `-race`.

## Comments and docs

- Comments are one to three lines saying why, then stop. Much of the existing
  code has paragraph-long comments; do not match that style and do not add to
  it.
- When behaviour changes, fix every comment, package doc, `README.md` and
  `docs/architecture.md` line that described the old behaviour in the same
  change. A confident comment that is no longer true is a bug.
- A behaviour change a consumer could notice gets a line under `Unreleased` in
  `CHANGELOG.md`.

## Git and releases

- Commit or push only when asked. Never commit to `master` directly; branch.
- Conventional commits with the package as scope: `fix(backup): …`,
  `feat(apitoken,fiberauth): …`, `docs: …`, `ci: …`, `build(deps): …`.
- Releases are minor tags only — `v0.x.0`, never a patch tag, even for a pure
  bug fix. After a tag is pushed, bump neokit in NeoGate's `server/`, `sdk/` and
  `plugins/` `go.mod`.
- Do not write plans or design notes into the repository unless asked.
