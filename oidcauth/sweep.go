package oidcauth

import (
	"context"
	"log/slog"
	"time"

	"github.com/neodata-io/neokit/jobs"
)

// Sweep timing. Daily is ample: expiry is enforced on every read (see
// [Policy.Live]), so a row that outlives its expiry authenticates nobody. The
// sweep is housekeeping, not a security control — its job is keeping the table
// from growing without bound.
const (
	SweepInterval = 24 * time.Hour
	sweepTimeout  = 30 * time.Second
)

// SweepJob builds the periodic expired-session sweep for a store that implements
// [ExpiredSweeper]. ok is false when the store cannot sweep, so a caller can skip
// scheduling it rather than run a job that does nothing.
//
// For a service mounting a Gate this is already done: fiberauth.New finds the
// sweep whether or not a login is configured, and fiberauth.Gate.Run runs it
// (neokit.App.Login starts that for you). Use this directly only when you are
// not mounting a Gate.
//
// It sweeps once at start, because a restart is exactly when a backlog has
// accumulated.
//
//	if job, ok := oidcauth.SweepJob(store, nil); ok {
//	    job.Start(ctx)
//	}
func SweepJob(store SessionStore, log *slog.Logger) (jobs.Job, bool) {
	sweeper, ok := store.(ExpiredSweeper)
	if !ok {
		return jobs.Job{}, false
	}
	if log == nil {
		log = slog.Default()
	}
	return jobs.Job{
		Name:       "oidc-session-sweep",
		Every:      SweepInterval,
		Timeout:    sweepTimeout, // a single indexed DELETE, so generous
		RunAtStart: true,
		Log:        log,
		Do: func(ctx context.Context) error {
			n, err := sweeper.DeleteExpiredSessions(ctx, time.Now().UTC())
			if err != nil {
				return err
			}
			if n > 0 {
				log.InfoContext(ctx, "swept expired sessions", "count", n)
			}
			return nil
		},
	}, true
}
