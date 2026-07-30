# neokit

Small, domain-free Go building blocks extracted from NeoGate, a private household
server project: structured logging, an HTTP client with retries and SSRF guarding,
GoFiber helpers, a stale-while-revalidate cache, and a SQLite migration runner.

Pre-1.0: the API may change between `v0.x` releases.

## Packages

| Package | What |
|---|---|
| `logx` | `slog` context handler, canonical `Err` attribute, request-id propagation |
| `httpc` | `NewHTTPClient`, `BaseClient`, retry transport, `APIError`, `Classify`, SSRF guard |
| `fiberx` | `{"error": …}` envelope, bind+validate, metrics/logging middleware, rate limiters |
| `cache` | stale-while-revalidate `GetOrFetch` |
| `sqlitex` | `PRAGMA user_version` append-only migration runner |
| `tracing` `metrics` | OpenTelemetry and Prometheus wiring |
| `safe` `ids` `clock` | goroutine recovery, id/token generation, injectable clock |

MIT.
