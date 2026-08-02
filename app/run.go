package app

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/neodata-io/neokit/lifecycle"
	"github.com/neodata-io/neokit/logx"
	"github.com/neodata-io/neokit/netx"
)

// Teardown budgets.
//
// The per-step bound is what stops one wedged Close from holding the process
// open; the HTTP drain has its own, because it means something different — how
// long a slow client is allowed to finish.
const (
	shutdownGraceTimeout = 30 * time.Second
	shutdownStepTimeout  = 15 * time.Second
	httpDrainTimeout     = 10 * time.Second
)

// Report renders the boot block: what this process actually is. Run prints it to
// stdout; it is exported so a caller can send it somewhere else instead.
func (a *App) Report() string {
	addr, _ := listenAddress(a.Cfg.BindAddr, a.Cfg.Port)
	return a.report(addr)
}

// Run starts the listener, waits for a termination signal or a fatal listener
// error, and unwinds the teardown stack. It blocks until the process is done.
//
// The three steps it pushes complete the order described on [App]:
//
//	streams → api → background-context
//	        → [the application's steps, reversed] → metrics-export → tracing
func (a *App) Run() error {
	addr, network := listenAddress(a.Cfg.BindAddr, a.Cfg.Port)

	// Here rather than in New: the report is only complete once the caller has
	// finished declaring its subsystems.
	fmt.Println(a.Report())

	// Buffered, so the listener goroutine cannot leak when Run returns on a
	// signal without ever reading from it.
	serverErr := make(chan error, 1)

	go func() {
		if err := a.HTTP.Listen(addr, fiber.ListenConfig{
			DisableStartupMessage: true, // the boot report replaces it
			ListenerNetwork:       network,
		}); err != nil {
			serverErr <- fmt.Errorf("api server: %w", netx.AddrInUseHint(err, a.Cfg.Port, "PORT"))
		}
	}()

	a.pushRunSteps()

	// SIGINT and SIGTERM — the two a terminal and a container runtime send.
	quit, stop := lifecycle.Signals(context.Background())
	defer stop()

	var fatal error
	select {
	case <-quit.Done():
		a.Log.Info("received signal, shutting down")
	case err := <-serverErr:
		a.Log.Error("server error, shutting down", logx.Err(err))
		fatal = err
	}
	// Restore the default disposition so a second signal terminates immediately
	// rather than queueing behind the drain.
	stop()

	shutdownErr := a.Close()

	// Prefer the fatal listener error, so a process that never came up exits
	// non-zero; otherwise surface whatever the teardown reported.
	if fatal != nil {
		return fatal
	}
	return shutdownErr
}

// pushRunSteps registers the teardown Run owns. Order matters: the stack unwinds
// in reverse, so the last pushed runs first.
func (a *App) pushRunSteps() {
	// After the API drain, not before: reversing the two lets a late request
	// start background work concurrently with the drain that waits for it.
	a.Shutdown.Push("background-context", func(context.Context) error {
		a.cancel()
		return nil
	})

	// Its own timeout rather than the stack's per-step bound, because the drain
	// means how long a slow client may take to finish.
	a.Shutdown.Push("api", func(context.Context) error {
		return a.HTTP.ShutdownWithTimeout(httpDrainTimeout)
	})

	// Released first of all, so the drain does not wait its full timeout on a
	// stream that would never end on its own.
	a.Shutdown.Push("streams", func(context.Context) error {
		a.closeDraining()
		return nil
	})
}

// Close runs the teardown stack and cancels the application context. It is
// idempotent, so a deferred Close covering the early-return paths is inert once
// Run has already torn down.
func (a *App) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGraceTimeout)
	defer cancel()

	err := a.Shutdown.Shutdown(ctx, shutdownStepTimeout)

	// Belt and braces for the paths that never reached Run: without its steps on
	// the stack, nothing else would cancel the context or release the streams.
	a.cancel()
	a.closeDraining()
	return err
}

// listenAddress returns a host:port net.Listen accepts and the Fiber network for
// it. Fiber defaults to tcp4, which preserves the all-IPv4 bind when BindAddr is
// empty but cannot bind an explicit IPv6 address; an explicit host uses tcp so
// either family resolves.
func listenAddress(bindAddr string, port int) (addr, network string) {
	host := strings.TrimSpace(bindAddr)
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	network = fiber.NetworkTCP4
	if host != "" {
		network = fiber.NetworkTCP
	}
	return net.JoinHostPort(host, fmt.Sprint(port)), network
}
