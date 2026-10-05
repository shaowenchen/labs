// Command labs is the laboratory service: it hands out short-lived applab and
// sandboxlab environments to anonymous callers, and keeps the environments
// underneath them alive by dispatching GitHub Actions workflows.
//
// It is one process with one job, so it has one command. Everything about how it
// behaves comes from the environment (see internal/config), which is where a
// deployment's settings are read from — there are no flags that would have to be
// kept in step with the compose file or the chart.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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

func main() {
	// A signal-cancelled context is what lets the reaper stop and the HTTP
	// server drain, rather than the process being killed mid-request.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var (
		listen      string
		printConfig bool
	)
	flag.StringVar(&listen, "listen", "", "address to bind, overriding LABS_LISTEN")
	flag.BoolVar(&printConfig, "print-config", true, "log the resolved configuration at startup")
	flag.Parse()

	if err := run(ctx, listen, printConfig); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, listen string, printConfig bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if listen != "" {
		cfg.Listen = listen
	}

	log := logging.New(cfg.LogLevel, os.Stderr)
	slog.SetDefault(log)
	log.Info("starting labs", "build", buildinfo.String())
	if printConfig {
		printResolved(cfg, log)
	}
	// The configuration problems are logged loudly and then served, rather than
	// exiting. See config.Load for why: a process that exits on incomplete
	// configuration cannot be told apart from one that crashed.
	if len(cfg.Problems) > 0 {
		log.Warn("this deployment is not configured yet and cannot offer labs; /readyz and /api/v1/config say what is missing",
			"problems", len(cfg.Problems))
		for _, p := range cfg.Problems {
			log.Warn("configuration", "problem", p)
		}
	}

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
		Targets: starterTargets(cfg),
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

	go reaper.New(manager, cfg.ReapInterval, log).Run(ctx)

	server := api.New(api.Deps{
		Config:  cfg,
		Service: manager,
		Limiter: ratelimit.New(cfg.RateLimitCount, cfg.RateLimitWindow),
		Log:     log,
	})

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("serving", "listen", cfg.Listen, "session_ttl", cfg.SessionTTL.String())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// A bounded drain: the process should stop, but not so abruptly that a
	// request in flight is cut off with no response at all.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("did not shut down cleanly", "error", err)
	}
	return nil
}

// starterTargets turns the configured environments into starter targets, using
// the same input-building the model owns so a workflow is dispatched
// identically however it is started.
func starterTargets(cfg config.Config) []gha.Target {
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
