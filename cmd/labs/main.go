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
	"github.com/shaowenchen/labs/internal/gha"
	"github.com/shaowenchen/labs/internal/logging"
	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/ratelimit"
	"github.com/shaowenchen/labs/internal/reaper"
	"github.com/shaowenchen/labs/internal/session"
	"github.com/shaowenchen/labs/internal/store"
)

func main() {
	// A signal-cancelled context is what lets the keeper and the reaper stop and
	// the HTTP server drain, rather than the process being killed mid-request.
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

	// One driver per kind the deployment actually runs. A kind with no driver
	// (sandboxlab, which is not implemented) is left out, and its environment
	// simply never reports ready — the configuration already lists that as a
	// problem, so this is not a second, silent failure.
	drivers := map[model.Kind]driver.Driver{}
	for _, env := range cfg.Envs {
		if _, ok := drivers[env.Kind]; ok {
			continue
		}
		if env.Kind == model.KindApplab {
			drivers[env.Kind] = applab.New(log)
		}
	}

	gh := gha.New(cfg.GitHubAPI, cfg.GitHubToken)

	manager := session.New(cfg, st, drivers, log)
	// For environments configured with no domain, the address is read from the
	// environment's own run log — the only way to learn a hostname that belongs
	// to whatever tunnel the deployment owns.
	manager.WithDiscovery(gha.NewDiscoverer(gh).Discover)

	// Keep the environments warm. Only started when the configuration can
	// actually dispatch: a keeper with no token or no repositories would fail
	// on every tick, which is noise that hides the configuration problem the
	// operator needs to see.
	if cfg.KeepWarm && cfg.Usable() {
		keeper := gha.NewKeeper(gha.KeeperConfig{
			Client:   gh,
			Targets:  keeperTargets(cfg),
			Interval: cfg.KeepWarmInterval,
			Lifetime: gha.LifetimeForRun(cfg.DispatchSessionHours),
			Margin:   cfg.RedispatchMargin,
			Log:      log,
		})
		go keeper.Run(ctx)
	} else if !cfg.KeepWarm {
		log.Info("LABS_KEEPWARM is off; environments are not being kept warm")
	}

	// Make the records a later provision needs exist ahead of the first request,
	// so the first caller does not pay for creating them. Best-effort: an
	// environment that is not up yet just logs a warning here, and Provision
	// ensures the slot itself when the time comes.
	go warmLoop(ctx, manager, cfg.KeepWarmInterval)

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

// keeperTargets turns the configured environments into keeper targets, using the
// same input-building the model owns so a workflow is dispatched identically
// however it is started.
func keeperTargets(cfg config.Config) []gha.Target {
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

// warmLoop ensures slots exist periodically, so a provision that follows an
// environment coming up does not have to create anything first.
func warmLoop(ctx context.Context, m *session.Manager, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	m.Warm(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Warm(ctx)
		}
	}
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
		"keep_warm", cfg.KeepWarm,
		"trusted_proxy", cfg.TrustedProxy,
		"max_sessions", cfg.SessionCeiling(),
		"max_sessions_per_ip", cfg.MaxSessionsPerIP,
		"environments", envs,
	)
}
