# Changelog

Changes a service built on neokit could notice. Releases are minor tags only
(`v0.x.0`).

## Unreleased

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
