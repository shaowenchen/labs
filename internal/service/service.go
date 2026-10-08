// Package service assembles the running service from a resolved configuration.
//
// The wiring lives here rather than in a command so that the two ways this is
// started — the long-lived process and the serverless server Vercel runs — build
// exactly the same thing. A second copy of this would be a second answer to
// "which driver, which starter, which discovery", and the two would drift.
package service

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/shaowenchen/labs/internal/api"
	"github.com/shaowenchen/labs/internal/buildinfo"
	"github.com/shaowenchen/labs/internal/config"
	"github.com/shaowenchen/labs/internal/driver"
	"github.com/shaowenchen/labs/internal/driver/applab"
	"github.com/shaowenchen/labs/internal/driver/sandboxlab"
	"github.com/shaowenchen/labs/internal/gha"
	"github.com/shaowenchen/labs/internal/logging"
	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/ratelimit"
	"github.com/shaowenchen/labs/internal/reaper"
	"github.com/shaowenchen/labs/internal/session"
	"github.com/shaowenchen/labs/internal/store"
)

// Service is the assembled service: the manager the reaper drives, and the HTTP
// handler over it.
type Service struct {
	Manager *session.Manager
	Server  *api.Server
}

// Build wires the components for a resolved configuration.
func Build(cfg config.Config, log *slog.Logger) *Service {
	st := store.New()

	// One driver per kind the deployment actually runs. Both kinds are
	// implemented: applab mints a per-app key, sandboxlab creates a sandbox with
	// its own two-hour clock.
	drivers := map[model.Kind]driver.Driver{}
	for _, env := range cfg.Envs {
		if _, ok := drivers[env.Kind]; ok {
			continue
		}
		switch env.Kind {
		case model.KindApplab:
			drivers[env.Kind] = applab.New(log)
		case model.KindSandboxlab:
			drivers[env.Kind] = sandboxlab.New(cfg.SessionTTL, log)
		}
	}

	gh := gha.New(cfg.GitHubAPI, cfg.GitHubToken)

	// The starter checks whether an environment's workflow has a run going and
	// dispatches one only when it does not. There is no timer: a repository runs
	// one environment at a time, so "is it up" is a question asked when a lab is
	// wanted, not something to poll for.
	starter := gha.NewStarter(gha.StarterConfig{
		Client:  gh,
		Targets: StarterTargets(cfg),
		Log:     log,
	})
	targetByEnv := map[string]gha.Target{}
	for _, t := range starter.Targets() {
		targetByEnv[t.ID] = t
	}

	manager := session.New(cfg, st, drivers, log)
	// For environments configured with no domain, the address is read from the
	// environment's own run log.
	manager.WithDiscovery(gha.NewDiscoverer(gh).Discover)
	// Creating a lab is what brings an environment up: the check-and-dispatch
	// runs then, and only then. A deployment that cannot dispatch does not get a
	// starter at all, so the request is answered as "not configured".
	if cfg.Usable() {
		manager.WithStarter(func(ctx context.Context, env model.Env) bool {
			t, ok := targetByEnv[env.ID]
			if !ok {
				return false
			}
			return starter.EnsureRunning(ctx, t)
		})
		// Status is decided in the order that matters: whether a run is active
		// first, then whether the service answers. An environment with no run
		// has nothing serving its address, so asking it anything returns a
		// tunnel error that says less than "no run is active".
		manager.WithRunnerCheck(func(ctx context.Context, env model.Env) (bool, string, bool) {
			t, ok := targetByEnv[env.ID]
			if !ok {
				return true, "", false
			}
			return starter.Running(ctx, t)
		})
	}

	server := api.New(api.Deps{
		Config:  cfg,
		Service: manager,
		Limiter: ratelimit.New(cfg.RateLimitCount, cfg.RateLimitWindow),
		Log:     log,
	})

	return &Service{Manager: manager, Server: server}
}

// StarterTargets turns the configured environments into starter targets, using
// the same input-building the model owns so a workflow is dispatched
// identically however it is started.
func StarterTargets(cfg config.Config) []gha.Target {
	targets := make([]gha.Target, 0, len(cfg.Envs))
	for _, env := range cfg.Envs {
		targets = append(targets, gha.Target{
			ID:       env.ID,
			Repo:     env.Repo,
			Workflow: env.Workflow,
			Ref:      env.Ref,
			Inputs:   env.DispatchInputs(cfg.DispatchSessionHours),
		})
	}
	return targets
}

// Config is the command-line configuration: the settings that come from flags
// rather than from the environment. Both commands take the same two, so both
// carry them and neither is a special case of the other.
type Config struct {
	// Listen overrides LABS_LISTEN (and PORT) for the address to bind.
	Listen string

	// PrintConfig logs the resolved configuration at startup.
	PrintConfig bool
}

// Runtime is an assembled service that owns a running process: the manager,
// the HTTP server, and the reaper and logger that go with the long-lived form.
type Runtime struct {
	*Service
	Config config.Config
	Server *http.Server
	Log    *slog.Logger
}

// Load resolves the configuration from the environment, applying the flags, and
// logs it.
//
// The configuration problems are logged loudly and then served, rather than
// exiting: a process that exits on incomplete configuration cannot be told apart
// from one that crashed. See config.Load for the same reasoning.
func Load(cfg Config) (config.Config, *slog.Logger, error) {
	resolved, err := config.Load()
	if err != nil {
		return config.Config{}, nil, err
	}
	if cfg.Listen != "" {
		resolved.Listen = cfg.Listen
	}

	log := logging.New(resolved.LogLevel, os.Stderr)
	slog.SetDefault(log)
	log.Info("starting labs", "build", buildinfo.String())
	// Worth a line: a generated key reaches only the environments this process
	// started, and someone whose lab will not open should hear that here rather
	// than have to infer it from a refused call.
	if resolved.GeneratedKey {
		log.Info("no LABS_ACTION_API_KEY is set, so a key was generated for this process; it reaches only the environments this process starts. Set LABS_ACTION_API_KEY to the repositories' key to reach ones already up")
	}
	if cfg.PrintConfig {
		printResolved(resolved, log)
	}
	if len(resolved.Problems) > 0 {
		log.Warn("this deployment is not configured yet and cannot offer labs; /readyz and /api/v1/config say what is missing",
			"problems", len(resolved.Problems))
		for _, p := range resolved.Problems {
			log.Warn("configuration", "problem", p)
		}
	}
	return resolved, log, nil
}

// NewRuntime builds the whole service and an HTTP server over it, ready to
// serve, and starts the reaper.
func NewRuntime(ctx context.Context, cfg Config, resolved config.Config, log *slog.Logger) *Runtime {
	svc := Build(resolved, log)

	// The reaper is a goroutine the process owns. It is why this is the
	// long-lived form: a serverless platform that starts the process per request
	// has no process for it to live in, and expiry is left to the environments
	// themselves — which sandboxlab does and applab does not. See api.Vercel for
	// the serverless form and what it drops.
	go reaper.New(svc.Manager, resolved.ReapInterval, log).Run(ctx)

	httpServer := &http.Server{
		Addr:              resolved.Listen,
		Handler:           svc.Server,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	return &Runtime{Service: svc, Config: resolved, Server: httpServer, Log: log}
}

// Serve runs the server until ctx is cancelled or the listener fails, then
// drains. It is the body of the long-lived process.
func (r *Runtime) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		r.Log.Info("serving", "listen", r.Config.Listen, "session_ttl", r.Config.SessionTTL.String())
		if err := r.Server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		r.Log.Info("shutting down")
	}

	// A bounded drain: the process should stop, but not so abruptly that a
	// request in flight is cut off with no response at all.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := r.Server.Shutdown(shutdownCtx); err != nil {
		r.Log.Warn("did not shut down cleanly", "error", err)
	}
	return nil
}

func printResolved(cfg config.Config, log *slog.Logger) {
	envs := make([]map[string]any, 0, len(cfg.Envs))
	for _, e := range cfg.Envs {
		envs = append(envs, map[string]any{
			"id":       e.ID,
			"kind":     string(e.Kind),
			"repo":     e.Repo,
			"workflow": e.Workflow,
			"domain":   e.Domain,
			"base":     e.BaseURL(),
			"capacity": e.Capacity,
			// Not the key: a resolved-configuration line is exactly the kind of
			// output that ends up pasted into an issue.
			"key_set": e.APIKey != "",
		})
	}
	log.Info("resolved configuration",
		"listen", cfg.Listen,
		"session_ttl", cfg.SessionTTL.String(),
		"trusted_proxy", cfg.TrustedProxy,
		"max_sessions", cfg.SessionCeiling(),
		"max_sessions_per_ip", cfg.MaxSessionsPerIP,
		"environments", envs,
	)
}
