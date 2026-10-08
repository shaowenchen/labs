// Command server runs the laboratory service as a plain HTTP server: it binds
// the address it is configured with and serves.
//
// It exists for Vercel. Vercel's Go framework preset detects one of main.go,
// cmd/api/main.go or cmd/server/main.go, runs it as a server, and gives it a
// PORT — and it routes every request to it with the original path intact, which
// is what lets the service's own routing see "/" and "/api/v1/labs" rather than
// the rewritten path a function would. cmd/labs already binds PORT when
// LABS_LISTEN is unset (see config.resolveListen), so this is not a second
// behaviour, only a second entry point.
//
// It is deliberately the same service as cmd/labs, wired by internal/service.
// What differs is only what a serverless platform does not give it. Vercel runs
// more than one instance and starts them cold, and this service was built to be
// one process: the session store is in memory, so two instances hold two
// different halves of the sessions and can hand the same slot out twice; and the
// applab keys it mints do not expire on their own, so without the reaper they
// live until the environment is replaced. The README's "On Vercel" section sets
// this out in full, because it is the thing to decide on before deploying here
// rather than a detail to discover afterwards.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/shaowenchen/labs/internal/service"
)

func main() {
	var cfg service.Config
	flag.StringVar(&cfg.Listen, "listen", "", "address to bind, overriding LABS_LISTEN and PORT")
	flag.BoolVar(&cfg.PrintConfig, "print-config", true, "log the resolved configuration at startup")
	flag.Parse()

	resolved, log, err := service.Load(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}

	svc := service.Build(resolved, log)

	// No reaper: the process is started per request and stopped when the request
	// is done, so a sweeper goroutine would be killed before its first tick.
	// Expiry is left to the environments, which sandboxlab does and applab does
	// not. No signal handling either — there is no long-lived process to drain.
	log.Info("serving", "listen", resolved.Listen, "mode", "serverless", "session_ttl", resolved.SessionTTL.String())
	if err := http.ListenAndServe(resolved.Listen, svc.Server); err != nil {
		log.Error("the server stopped", "error", err)
		os.Exit(1)
	}
}
