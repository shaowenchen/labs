package gha

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Target is one environment the keeper keeps alive: the workflow that brings it
// up, and the inputs that pick which environment it is.
type Target struct {
	// ID names the environment, for logs and for the per-target lock.
	ID string

	// Repo is "owner/repo", Workflow the workflow file, Ref the git ref.
	Repo     string
	Workflow string
	Ref      string

	// Inputs are the workflow_dispatch inputs. They must match what the
	// workflow declares, choices included.
	Inputs map[string]string
}

// Keeper dispatches successor runs so an environment is always coming back.
//
// It exists because a GitHub-hosted job has a ceiling of six hours and an
// environment is expected to be reachable for much longer than that. Each
// dispatched run runs for hours and ends; shortly before it does, the keeper
// queues its successor, so the environment is replaced rather than merely
// restarted on the next request.
type Keeper struct {
	client   *Client
	targets  []Target
	interval time.Duration

	// lifetime is how long a dispatched run is expected to last, and margin how
	// early its successor is queued.
	lifetime time.Duration
	margin   time.Duration

	// bootCeiling is how long a run may take to bring an environment up before
	// it is treated as stuck. Past it, a run that has still announced nothing is
	// cancelled and a fresh one dispatched.
	bootCeiling time.Duration

	log *slog.Logger
	now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// KeeperConfig is what a keeper needs to run.
type KeeperConfig struct {
	Client   *Client
	Targets  []Target
	Interval time.Duration
	Lifetime time.Duration
	Margin   time.Duration
	Log      *slog.Logger
}

// NewKeeper builds a keeper.
func NewKeeper(c KeeperConfig) *Keeper {
	return &Keeper{
		client:      c.Client,
		targets:     c.Targets,
		interval:    c.Interval,
		lifetime:    c.Lifetime,
		margin:      c.Margin,
		bootCeiling: 20 * time.Minute,
		log:         c.Log,
		now:         time.Now,
		locks:       map[string]*sync.Mutex{},
	}
}

// Run ticks until ctx is cancelled.
func (k *Keeper) Run(ctx context.Context) {
	if k.interval <= 0 {
		k.log.Info("the keeper is disabled; environments are not being kept warm")
		return
	}
	k.log.Info("the keeper is running", "interval", k.interval, "targets", len(k.targets))
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			k.Tick(ctx)
		}
	}
}

// Tick runs the state machine once for every target.
func (k *Keeper) Tick(ctx context.Context) {
	for _, t := range k.targets {
		if ctx.Err() != nil {
			return
		}
		k.tickTarget(ctx, t)
	}
}

// EnsureRunning makes sure the environment is coming up and reports whether a
// run is now active or was just dispatched.
//
// It is the same decision Tick makes, exposed for the request path: on a host
// with no long-lived process to run the keeper, a request is the only thing
// that can start an environment, and it should — a caller asking for a lab
// should get one, even if nobody was keeping the environment warm.
//
// true means "a run is up, or is now queued" — the caller should look again
// shortly. false means nothing was dispatched and nothing is running: either
// listing runs failed, or there is no target for the environment.
func (k *Keeper) EnsureRunning(ctx context.Context, t Target) bool {
	lock := k.lockFor(t.ID)
	lock.Lock()
	defer lock.Unlock()

	runs, err := k.client.Runs(ctx, t.Repo, t.Workflow, t.Ref, 10)
	if err != nil {
		// A listing that fails says nothing about whether a run is up. Answer no
		// rather than dispatch: a blind dispatch could cancel a live run's
		// successor, and the caller can try again.
		k.log.Warn("could not list runs", "env", t.ID, "repo", t.Repo, "error", err)
		return false
	}

	var (
		hasQueued bool
		newestRun *Run
	)
	for i := range runs {
		r := runs[i]
		if r.Status == "queued" {
			hasQueued = true
		}
		if newestRun == nil || r.begun().After(newestRun.begun()) {
			newestRun = &runs[i]
		}
	}

	switch {
	case hasQueued:
		return true
	case newestRun != nil && newestRun.Running():
		// Only a run that has gone far past a boot is replaced. Restarting a
		// boot in progress would be a loop, not a recovery.
		if k.cancelStuck(ctx, t, newestRun) {
			return k.dispatch(ctx, t, "the previous run was stuck")
		}
		return true
	default:
		return k.dispatch(ctx, t, "a request needed an environment and none was running")
	}
}

// cancelStuck cancels the given run if it has been going long enough that it is
// not merely slow, and reports whether it did.
//
// It is the "there is already one running; drop it and start fresh" rule, with
// the one guard that makes it safe: a run is only cancelled past the boot
// ceiling. Cancelling on sight would restart a boot that is in progress, every
// time a request or a tick looked, and nothing would ever finish coming up.
func (k *Keeper) cancelStuck(ctx context.Context, t Target, run *Run) bool {
	if run == nil {
		return false
	}
	age := k.now().Sub(run.begun())
	if age < k.bootCeiling {
		return false
	}
	k.log.Info("cancelling a run that has not come up", "env", t.ID, "run", run.ID, "age", age.Truncate(time.Second))
	if err := k.client.CancelRun(ctx, t.Repo, run.ID); err != nil {
		k.log.Warn("could not cancel a stuck run", "env", t.ID, "run", run.ID, "error", err)
		return false
	}
	return true
}

// tickTarget is the state machine for one environment.
//
// The rule that the whole thing turns on: never dispatch while a run is already
// queued. GitHub's default concurrency queue holds a single pending run and a
// new dispatch replaces it, so a keeper that dispatched on every tick would
// cancel its own successor, forever, and the environment would never come back.
// Everything else here is the ordinary case.
func (k *Keeper) tickTarget(ctx context.Context, t Target) {
	lock := k.lockFor(t.ID)
	lock.Lock()
	defer lock.Unlock()

	runs, err := k.client.Runs(ctx, t.Repo, t.Workflow, t.Ref, 10)
	if err != nil {
		// A listing that fails is not a reason to dispatch: an environment may
		// be perfectly up and this is a transient GitHub error, and a blind
		// dispatch now could cancel a live run's successor.
		k.log.Warn("could not list runs", "env", t.ID, "repo", t.Repo, "error", err)
		return
	}

	var (
		hasQueued bool
		newestRun *Run
	)
	for i := range runs {
		r := runs[i]
		if r.Status == "queued" {
			hasQueued = true
		}
		if newestRun == nil || r.begun().After(newestRun.begun()) {
			newestRun = &runs[i]
		}
	}

	switch {
	case hasQueued:
		// A successor is already waiting; dispatching again would cancel it.
		k.log.Debug("a run is already queued", "env", t.ID)
		return

	case newestRun != nil && newestRun.Running():
		// A run is live. Usually leave it alone; queue a successor once it is
		// near its expected end. But a run that has gone far past a boot without
		// ever coming up is stuck, and is dropped so a fresh one can start.
		if k.cancelStuck(ctx, t, newestRun) {
			k.dispatch(ctx, t, "the previous run was stuck")
			return
		}
		age := k.now().Sub(newestRun.begun())
		if age < k.lifetime-k.margin {
			k.log.Debug("a run is in progress", "env", t.ID, "age", age.Truncate(time.Second))
			return
		}
		k.dispatch(ctx, t, "the current run is near its end")

	default:
		// Nothing is running and nothing is queued: the environment is down.
		k.dispatch(ctx, t, "no run is active")
	}
}

// dispatch starts a run and reports whether the dispatch was accepted. It logs
// the outcome rather than returning an error: a failed dispatch is a thing to
// retry on the next tick, or on the next request, not a reason to stop.
//
// It does not wait for the run to appear. The dispatch POST is what matters and
// it returns in about a second; waiting for the run to show up in the listing
// can take a minute, and on the request path that wait is a page that hangs.
// The run is found by the next listing instead, where its absence at worst
// means one more dispatch attempt — which never cancels a queued successor.
func (k *Keeper) dispatch(ctx context.Context, t Target, why string) bool {
	k.log.Info("dispatching a run", "env", t.ID, "repo", t.Repo, "workflow", t.Workflow, "reason", why)
	if err := k.client.Dispatch(ctx, t.Repo, t.Workflow, t.Ref, t.Inputs); err != nil {
		k.log.Warn("could not dispatch a run", "env", t.ID, "repo", t.Repo, "error", err)
		return false
	}
	k.log.Info("dispatched", "env", t.ID, "repo", t.Repo)
	return true
}

func (k *Keeper) lockFor(id string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok := k.locks[id]
	if !ok {
		l = &sync.Mutex{}
		k.locks[id] = l
	}
	return l
}

// LifetimeForRun is the expected wall clock of a run dispatched with the given
// session_hours input, matching the workflows' own timeout chain.
//
// It only has to be close: it decides when a successor is queued, and the margin
// absorbs the difference between this estimate and the workflow's real ceiling.
func LifetimeForRun(sessionHours string) time.Duration {
	switch sessionHours {
	case "1":
		return time.Hour
	case "2":
		return 2 * time.Hour
	case "4":
		return 4 * time.Hour
	case "unlimited":
		// The workflows cap an "unlimited" run at the runner's ceiling.
		return 6 * time.Hour
	default:
		return 4 * time.Hour
	}
}
