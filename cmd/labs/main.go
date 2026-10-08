// Command labs is the laboratory service: it hands out short-lived applab and
// sandboxlab environments to anonymous callers, and keeps the environments
// underneath them alive by dispatching GitHub Actions workflows.
//
// It is one process with one job, so it has one command. Everything about how it
// behaves comes from the environment (see internal/config), which is where a
// deployment's settings are read from — there are no flags that would have to be
// kept in step with the compose file or the chart, only the two below.
//
// This is the long-lived form: a process that serves and reaps. The serverless
// form — a Go server Vercel runs from cmd/server, for a platform that starts a
// process per request — wires the same components in internal/service.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/shaowenchen/labs/internal/service"
)

func main() {
	// A signal-cancelled context is what lets the reaper stop and the HTTP
	// server drain, rather than the process being killed mid-request.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var cfg service.Config
	flag.StringVar(&cfg.Listen, "listen", "", "address to bind, overriding LABS_LISTEN")
	flag.BoolVar(&cfg.PrintConfig, "print-config", true, "log the resolved configuration at startup")
	flag.Parse()

	if err := run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg service.Config) error {
	resolved, log, err := service.Load(cfg)
	if err != nil {
		return err
	}
	return service.NewRuntime(ctx, cfg, resolved, log).Serve(ctx)
}
