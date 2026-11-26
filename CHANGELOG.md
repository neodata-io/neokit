# Changelog

Changes a service built on neokit could notice. Releases are minor tags only
(`v0.x.0`).

## Unreleased

### Security

- **`app`: with no `CORS_ORIGINS`, no other origin may read the API.** Fiber
  read an empty list as "every origin", so an app built without origins
  answered `Access-Control-Allow-Origin: *`.
- **`fiberx`, `app`, `tracing`: a raw request path no longer reaches a log or a
  span.** The "request failed" and "panic recovered" lines name the route
  template, and server spans drop `url.path`; `http.route` remains.
- **`app`: without `METRICS_TOKEN`, `/metrics` refuses a request forwarded by a
  proxy in `TRUSTED_PROXIES`** with 403. A direct scrape is still served; set
  the token to scrape through the proxy.
- `tracing`, `metrics`: the startup line names the OTLP endpoint's host only.

### Changed

- **`config`: `CORS_ORIGINS` has no default.** It was `http://localhost:3000`;
  a local frontend sets it explicitly.
- `app`: a client's `X-Request-Id` is kept only if it is at most 64 letters,
  digits, `-`, `_` or `.`; anything else is replaced with a generated id.
- `fiberx.RateLimiter`: an IPv6 client is counted by its /64.
- **`oidcauth`: a blank `OwnerGroup` now makes nobody an owner.** It used to
  make every signed-in user one. Set the group whose members should be owners;
  `fiberauth.New` warns at boot when a login has none.
- **`oidcauth`: an email the provider marks `email_verified: false` is
  dropped** — `Identity.Email` is empty for it.
- **`httpc.RedactURL` and `httpc.Redact` keep only `scheme://host[:port]`.**
  The path is dropped too, because bot tokens and webhook secrets ride in it;
  `BaseClient` errors and `DebugRing` entries no longer show the path.
- **`httpc` traced clients record `url.full` as `scheme://host` only.** Do not
  set `HTTPOptions.Tracing` on a client that fetches caller-supplied URLs: the
  host is a span name and a metric label.
- **The SSRF guard refuses more ranges.** Both policies refuse IPv4-compatible
  (`::/96`) and SIIT (`::ffff:0:0:0/96`) addresses; `IsPrivateAddr` also
  refuses `fec0::/10`, Teredo `2001::/32`, `198.18.0.0/15` and `192.0.0.0/24`.
- `safe.Pool`: the doc now says that `Limit` bounds running tasks only; the
  queue of waiting tasks is unbounded.

### Added

- `webpush.Keys` implements `String`, `GoString` and `slog.LogValuer`, printing
  the private key as `[REDACTED]`. JSON encoding is unchanged.

### Fixed

- `oidcauth`: an issuer published with a trailing slash (Authentik, Auth0) can
  sign in. Discovery accepts the configured issuer with or without it.
- `fiberauth`: a renewed session re-sends its cookie with the new expiry, so an
  active user is no longer signed out at the original TTL when `TTL` is shorter
  than `MaxLifetime`.
- `fiberauth`: the stored User-Agent no longer aliases Fiber's request buffer,
  which let a store that keeps the session see a later request's value.
- `session.Policy.Live`: a session with no `ExpiresAt` is not live.
- **`backup`: a backup file dated in the future no longer gets each day's
  backup deleted as it is written.** Pruning ranks only backups dated up to
  today.
- **`backup`: two services writing to one directory no longer delete or
  publish each other's unfinished snapshot.** Snapshots go to a random temp
  name, and a leftover temp is removed only once it is over an hour old.
- `cache`: a background refresh is drained by `app.Run` at shutdown instead of
  running on against closed resources, and its panic log no longer includes
  the cache key.
- `capset`: `All` and `ByName` return copies, so a caller that writes to the
  result no longer changes it for every other caller. The package no longer
  uses reflection.
- `tracing`, `fiberx`: a span records the status the client got for a returned
  error (a mapped 404, a 500) instead of 200.
- `fiberx.MetricsAndLogger`: a panicking request gets its `http` summary line
  and duration sample, with status 500.

## v0.20.0 — 2026-11-25

### Fixed

- `httpc.Image`: the error for a response that is not an image is now
  `errs.ErrNotFound`, so a caller proxying art can answer 404. In v0.19.0 it
  was an error nothing could recognise, which a host rendered as a 500.

## v0.19.0 — 2026-11-25

Fixes from a full security and correctness review. Read **Changed** before
upgrading: several defaults are stricter.

### Changed

- **`fiberauth`: API tokens expire after a year by default.**
  `Options.TokenMaxAge` unset now means `DefaultTokenMaxAge`; a negative value
  restores "no expiry". Tokens already issued keep the expiry they have.
- **`fiberx.RateLimiter` keys on `c.IP()`** and no longer reads
  `X-Forwarded-For` itself. Behind a reverse proxy, set `TRUSTED_PROXIES` or
  every caller shares one budget. `RateLimiterByPeer` is now the same limiter
  and is deprecated.
- **`sqlitex.Open`: transactions take the write lock when they begin**
  (`_txlock=immediate`). Pass `sql.TxOptions{ReadOnly: true}` for a transaction
  that only reads, or it queues behind writers.
- **New database, backup and snapshot files are `0600`, new directories `0700`.**
  Existing files and directories keep their mode. A sidecar running as another
  user can no longer read a newly created database.
- **`tracing`: a client's `traceparent` is honoured only from a trusted proxy**
  (`TRUSTED_PROXIES`). Other requests start a fresh trace.
- **`httpc`: redirects to another host keep only negotiation and trace
  headers,** and a redirect that would replay a request body to another host is
  refused. `notify` senders follow no redirects at all.
- **`httpc`: the address guard refuses more ranges** — carrier-grade NAT
  (`100.64.0.0/10`), `0.0.0.0/8`, `240.0.0.0/4`, cloud metadata addresses outside
  link-local, and NAT64/6to4 addresses that embed a refused IPv4 address.
- **`httpc`: a guard refusal, a certificate failure and an unsupported scheme
  are no longer retried** and classify as `FaultUnknown`, not
  `FaultUnavailable`.
- **`httpc.Image` refuses a response that is not `image/*`.**
- **`httpc.RetryConfig`: `MaxDelay <= 0` means the 2 s default** rather than
  "sleep for whatever `Retry-After` says".
- **`webpush/delivery`: a push endpoint must be an `https` URL on a public
  address;** redirects are not followed.
- **`webpush.NormalizeSubject` rejects a display name or URL userinfo.**
- **`jobs`: `Job.Start` and `Daily.Start` panic at the call site** on an
  unusable job, instead of retrying a recovered panic every five seconds.
- **`lifecycle`: a shutdown step that ignores its context is abandoned** when
  its budget runs out and reported as an error; later steps still run.
- **`backup`: only `<prefix><YYYY-MM-DD>.db` files are listed and pruned.**
- **`session.SQLite`: a row whose `created_at` or `expires_at` will not parse
  no longer signs anyone in.**

### Added

- `fiberauth.Options.Required`: with no login configured, the guards answer
  `503 login_not_configured` instead of passing every request through.
- `session.Session.Issuer`: sessions record the identity provider they were
  made under, and a gate refuses one made under another. The SQLite store adds
  the column itself; rows from before it run out on their own lifetime.
- `fiberauth.DefaultTokenMaxAge`.

### Fixed

- `backup`: a failed or interrupted snapshot is no longer accepted as today's
  backup, and pruning can no longer delete files it did not write.
- `sqlitex`: a transaction that reads and then writes no longer fails at once
  with "database is locked"; two processes starting together cannot apply a
  migration step twice; a path containing `#`, `?` or `%` opens that file.
- `tracing`: spans are named by route template (every span was `GET /`), and
  no longer keep strings from Fiber's pooled request buffer. With tracing off
  the middleware allocates nothing.
- `health`: readiness probes that overlap share one check instead of reporting
  a healthy service as not ready.
- `cache`: callers waiting on a cold key share its failure instead of each
  calling the upstream again; `Invalidate` of an unknown key evicts nothing.
- `apitoken`: expired tokens no longer count toward the per-person limit.
- `httpc`: `BaseClient` errors no longer carry a URL's query string; a `204`
  with a target to decode into is success. `notify` and `webpush/delivery`
  errors and logs name the target by host only.
- `jobs`: no run starts after cancellation; `Daily` cannot run twice when the
  clock steps back.
- `fiberx`: `WriteError` fills a missing status and code on an `APIError`;
  `IfMatchVersion` no longer reads `007` or `+7` as version 7.
- `disk.Usage` of a directory measures that directory's filesystem.
