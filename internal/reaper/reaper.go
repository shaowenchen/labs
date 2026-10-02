// Package reaper releases sessions whose time is up.
//
// It is a loop rather than a scheduled job for the same reason the sibling
// projects use one: the work is small, it must not be forgotten to be enabled,
// and a goroutine owns it for exactly as long as the process lives.
//
// It also runs the reconciliation pass, less often, because a session that was
// never recorded — the service died between minting a credential and storing it
// — is invisible to the expiry sweep and can only be found by looking at what
// the environment is actually running against what this service believes.
package reaper

import (
	"context"
	"log/slog"
	"time"
)

// Expirer is the part of the session manager the reaper uses.
type Expirer interface {
	// Expire releases every session whose time is up and reports how many.
	Expire(ctx context.Context) (int, error)
	// Reconcile ends anything an environment is running that is not a live
	// session of this service.
	Reconcile(ctx context.Context) error
}

// Reaper releases expired sessions and reconciles on an interval.
type Reaper struct {
	exp   Expirer
	log   *slog.Logger
	tick  time.Duration
	every int // reconcile every N ticks
	count int
}

// New builds a reaper. An interval of zero or less disables the expiry sweep,
// which leaves expiry to the environments themselves — fine for sandboxlab,
// which reaps its own sandboxes, and not fine for applab, whose keys do not
// expire on their own.
func New(exp Expirer, interval time.Duration, log *slog.Logger) *Reaper {
	return &Reaper{exp: exp, log: log, tick: interval, every: 20}
}

// Run sweeps until ctx is cancelled.
func (r *Reaper) Run(ctx context.Context) {
	if r.tick <= 0 {
		r.log.Info("the reaper is disabled; sessions will not be released on their own")
		return
	}
	r.log.Info("the reaper is running", "interval", r.tick)

	// Once at startup, before the first tick: a service that was down long
	// enough for sessions to expire should clear them immediately rather than
	// wait a tick, and the reconcile that follows puts it back in step with the
	// environments.
	r.Sweep(ctx)

	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep runs one expiry pass and, every so often, a reconcile.
//
// A failure to release one session does not stop the pass — the next tick is
// thirty seconds away and the sessions already released are released.
func (r *Reaper) Sweep(ctx context.Context) {
	n, err := r.exp.Expire(ctx)
	if err != nil {
		r.log.Warn("the expiry sweep hit an error", "error", err)
	}
	if n > 0 {
		r.log.Info("released expired sessions", "count", n)
	}

	r.count++
	if r.count%r.every != 0 {
		return
	}
	if err := r.exp.Reconcile(ctx); err != nil {
		r.log.Warn("the reconcile pass hit an error", "error", err)
	}
}
