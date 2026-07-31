// Package debugserver puts a process's diagnostics — Prometheus metrics and,
// optionally, pprof — on a listener of their own.
//
// The separate listener is the whole point. Mounted on the application's port
// these endpoints inherit its exposure, and /debug/pprof will hand a heap dump
// (with whatever secrets are live in it) to anyone who can reach the app. On
// their own port they can be bound to loopback and reached through an SSH or
// VPN tunnel, so the blast radius is a deployment decision rather than a routing
// one.
//
// What it replaces is twenty lines of assembly that every service writes once:
// a second [http.Server], a mux, promhttp, five pprof registrations, and a
// ReadHeaderTimeout. Individually obvious, and each omission is quiet — a
// missing timeout is invisible until a stuck client pins a connection, and a
// missing /debug/pprof/cmdline is invisible until the day you need it.
//
// # pprof is opt-in, and importing this package is not free
//
// Set [Config.Pprof] to mount the profiling endpoints. They are off by default
// because exposing them is a security decision the caller owns.
//
// The caveat that cannot be designed away: mounting them requires importing
// net/http/pprof, whose init registers those same handlers on
// [http.DefaultServeMux]. That is unconditional and happens even with Pprof
// false. It costs nothing for a service that serves an explicit handler — which
// is nearly all of them — but a service that serves DefaultServeMux (the nil
// handler in [http.ListenAndServe]) will publish pprof on that port merely
// because something in its build imported this package.
package debugserver

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// defaultReadHeaderTimeout bounds how long a client may take to send its request
// headers. It is applied when [Config.ReadHeaderTimeout] is zero rather than
// left unset, because the unset value is unbounded: a client that opens a
// connection and sends nothing holds it for the life of the process, and it
// takes very few of those to make the diagnostics port unreachable exactly when
// it is being consulted.
const defaultReadHeaderTimeout = 10 * time.Second

// Config describes the diagnostics listener. Only Addr is required.
type Config struct {
	// Addr is the listen address, e.g. ":9090" or "127.0.0.1:9090". Prefer the
	// loopback form in production and reach it through a tunnel.
	Addr string

	// Pprof mounts /debug/pprof/* — see the package doc before enabling it on
	// any address that is not loopback.
	Pprof bool

	// Gatherer supplies the metrics served at /metrics. Nil means
	// [prometheus.DefaultGatherer], which is what a codebase using promauto is
	// already registering into.
	Gatherer prometheus.Gatherer

	// ReadHeaderTimeout bounds the request-header read. Zero means
	// defaultReadHeaderTimeout; there is deliberately no way to ask for
	// unbounded.
	ReadHeaderTimeout time.Duration

	// ErrorLog receives the http.Server's own errors. Nil means the standard
	// library's default, which writes to the log package.
	ErrorLog *slog.Logger
}

// New builds the diagnostics server. It binds nothing — pass it to [Serve] to
// start, and its Shutdown method to whatever owns teardown ordering (it already
// has the shape of a neokit lifecycle.Step).
func New(cfg Config) *http.Server {
	gatherer := cfg.Gatherer
	if gatherer == nil {
		gatherer = prometheus.DefaultGatherer
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))

	if cfg.Pprof {
		// Index also serves every profile that has no dedicated handler (/heap,
		// /goroutine, /allocs), which is why the prefix pattern comes first and
		// the four named ones are additions rather than the whole set.
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	readHeaderTimeout := cfg.ReadHeaderTimeout
	if readHeaderTimeout <= 0 {
		readHeaderTimeout = defaultReadHeaderTimeout
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	if cfg.ErrorLog != nil {
		srv.ErrorLog = slog.NewLogLogger(cfg.ErrorLog.Handler(), slog.LevelWarn)
	}
	return srv
}

// Serve runs srv until it stops, and blocks. It returns nil when the stop was a
// [http.Server.Shutdown] or Close, and the listen error otherwise.
//
// That distinction is the reason to call this instead of ListenAndServe
// directly: ListenAndServe reports the caller's own graceful shutdown as
// [http.ErrServerClosed], so the usual `go func() { errc <- srv.ListenAndServe()
// }()` sends a non-nil error down the fatal path on every clean exit. Callers
// either remember to special-case it here or ship a server that cannot exit
// zero.
//
// Wrap the returned error in netx.AddrInUseHint to turn the kernel's terse
// "address already in use" into a message naming the port and the setting that
// would move it.
func Serve(srv *http.Server) error {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
