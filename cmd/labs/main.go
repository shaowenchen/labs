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
	// The API package logs failures through the standard logger, so the
	// configured one has to be the default as well as the one passed down — or
	// a request failure would be logged at a level the deployment did not ask
	// for, by a handler that never saw this logger.
	slog.SetDefault(log)
	log.Info("starting labs", "build", buildinfo.String())
	if printConfig {
		printResolved(cfg, log)
	}

	st, err := store.Open(cfg.StateFile)
	if err != nil {
		return err
	}

	// One driver per kind the deployment actually runs. Built here rather than
	// inside the manager so a kind with no driver is a startup error rather than
	// a request-time surprise.
	drivers := map[model.Kind]driver.Driver{}
	for _, env := range cfg.Envs {
		if _, ok := drivers[env.Kind]; ok {
			continue
		}
		switch env.Kind {
		case model.KindApplab:
			drivers[env.Kind] = applab.New(log)
		case model.KindSandboxlab:
			return fmt.Errorf("sandboxlab is not implemented yet; configure only applab environments")
		}
	}

	manager := session.New(cfg, st, drivers, log)

	// Keep the environments warm. When it is off, an environment is started on
	// demand by this same client the first time a request needs one — which the
	// service does not do yet, so with the keeper off an environment someone
	// else started is used and none is started otherwise.
	if cfg.KeepWarm {
		targets := keeperTargets(cfg)
		keeper := gha.NewKeeper(gha.KeeperConfig{
			Client:   gha.New(cfg.GitHubAPI, cfg.GitHubToken),
			Targets:  targets,
			Interval: cfg.KeepWarmInterval,
			Lifetime: gha.LifetimeForRun(cfg.DispatchSessionHours),
			Margin:   cfg.RedispatchMargin,
			Log:      log,
		})
		go keeper.Run(ctx)
	} else {
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
		"state_file", cfg.StateFile,
		"session_ttl", cfg.SessionTTL.String(),
		"keep_warm", cfg.KeepWarm,
		"trusted_proxy", cfg.TrustedProxy,
		"max_sessions", cfg.SessionCeiling(),
		"max_sessions_per_ip", cfg.MaxSessionsPerIP,
		"environments", envs,
	)
}
