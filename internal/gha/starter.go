package gha

import (
	"context"
	"log/slog"
	"sync"
)

// Target is one environment the starter drives: the workflow that brings it up,
// and the inputs that pick which environment it is.
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

// Starter makes sure an environment's run is up, dispatching one only when
// nothing is running.
//
// There is no timer in here, deliberately. A GitHub repository runs one
// environment at a time — its workflow's concurrency group allows a single run —
// so "keep it warm" is not something to do on a schedule: it is a question
// asked at the moment an environment is needed. Check first; if something is
// running, that is the environment and it is used as it is; only if nothing is
// running is a run dispatched.
//
// It being stateless about age is the point. A run already going might be one
// this process dispatched a minute ago or one a person started by hand this
// morning — both are the environment, and both are used rather than replaced.
type Starter struct {
	client  *Client
	targets []Target
	log     *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// StarterConfig is what a starter needs.
type StarterConfig struct {
	Client  *Client
	Targets []Target
	Log     *slog.Logger
}

// NewStarter builds a starter.
func NewStarter(c StarterConfig) *Starter {
	return &Starter{
		client:  c.Client,
		targets: c.Targets,
		log:     c.Log,
		locks:   map[string]*sync.Mutex{},
	}
}

// EnsureRunning makes sure the environment is up and reports whether a run is
// now active or was just dispatched.
//
// true means "a run is up, or is now queued" — the caller should look again
// shortly. false means nothing is running and nothing could be dispatched:
// either listing the runs failed, or there is no target for the environment.
func (k *Starter) EnsureRunning(ctx context.Context, t Target) bool {
	// The check and the dispatch are one critical section per environment, so
	// two callers arriving together cannot both see "nothing running" and both
	// dispatch — the second would cancel the first's run.
	lock := k.lockFor(t.ID)
	lock.Lock()
	defer lock.Unlock()

	runs, err := k.client.Runs(ctx, t.Repo, t.Workflow, t.Ref, 10)
	if err != nil {
		// A listing that fails says nothing about whether a run is up. Answer no
		// rather than dispatch: a blind dispatch could cancel a live run, and the
		// caller can try again.
		k.log.Warn("could not list runs", "env", t.ID, "repo", t.Repo, "error", err)
		return false
	}

	for i := range runs {
		// Queued or in progress: the environment is there or is coming, and it is
		// this one — a second dispatch would cancel it rather than add to it.
		if runs[i].Running() {
			k.log.Debug("a run is already active", "env", t.ID, "repo", t.Repo, "run", runs[i].ID, "status", runs[i].Status)
			return true
		}
	}

	return k.dispatch(ctx, t, "nothing was running")
}

// Targets returns the environments this starter drives.
func (k *Starter) Targets() []Target { return k.targets }

// dispatch starts a run and reports whether the dispatch was accepted. It logs
// the outcome rather than returning an error: a failed dispatch is a thing to
// retry on the next request, not a reason to fail the caller.
//
// It does not wait for the run to appear. The dispatch POST is what matters and
// it returns in about a second; waiting for the run to show up in the listing
// can take a minute, and on the request path that wait is a page that hangs.
func (k *Starter) dispatch(ctx context.Context, t Target, why string) bool {
	k.log.Info("dispatching a run", "env", t.ID, "repo", t.Repo, "workflow", t.Workflow, "reason", why)
	if err := k.client.Dispatch(ctx, t.Repo, t.Workflow, t.Ref, t.Inputs); err != nil {
		k.log.Warn("could not dispatch a run", "env", t.ID, "repo", t.Repo, "error", err)
		return false
	}
	k.log.Info("dispatched", "env", t.ID, "repo", t.Repo)
	return true
}

func (k *Starter) lockFor(id string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok := k.locks[id]
	if !ok {
		l = &sync.Mutex{}
		k.locks[id] = l
	}
	return l
}
