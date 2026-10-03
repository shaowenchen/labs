package gha

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/shaowenchen/labs/internal/model"
)

// urlPattern finds the addresses a debugger run prints. GitHub renders a
// `::notice` line as `::notice title=...::<text>`, and the environment's summary
// prints the console address as `<url>`, so a bare https URL in the log is what
// both forms reduce to.
var urlPattern = regexp.MustCompile(`https://[a-zA-Z0-9._-]+(?::[0-9]+)?(/[^\s"'<>)` + "`" + `]*)?`)

// ExtractBaseURL finds an environment's address in a run's log and returns it as
// scheme://host, having confirmed its path is the deployment's base path.
//
// It is how this service learns where an environment lives without being told:
// the debugger workflows print the address they came up at (the run's summary
// carries "Open the console: <url>"), and reading it back is the only way to
// know a hostname that belongs to whatever tunnel the deployment owns. The base
// path is the discriminator — a log line mentions github.com, api.github.com and
// the tunnel both, and only the environment's own address includes the path the
// deployment is served under.
func ExtractBaseURL(log, basePath string) (string, bool) {
	basePath = strings.TrimRight(basePath, "/")
	var found string
	for _, raw := range urlPattern.FindAllString(log, -1) {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		if basePath != "" && !strings.HasPrefix(u.Path, basePath) {
			continue
		}
		found = u.Scheme + "://" + u.Host
		// The console address is exactly the base path, not something under it
		// (an app is at <base>/apps/<name>), so an exact match ends the search.
		if basePath == "" || u.Path == basePath {
			break
		}
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// Discoverer finds an environment's address by reading the log of the run that
// brought it up.
//
// It is how a deployment needs no domain configured at all. The debugger
// workflows print the address they came up at — into the run's log and its
// summary — and that address is the only place a hostname chosen by a tunnel
// can be learned without someone typing it in.
//
// The address is discovered once and cached. It is stable for the life of the
// run, and a run lasts hours, so re-reading the log on every probe would be
// thousands of pointless requests for a value that has not changed.
type Discoverer struct {
	client *Client

	mu    sync.Mutex
	cache map[string]string // env id -> scheme://host
}

// NewDiscoverer returns a discoverer using the given client.
func NewDiscoverer(c *Client) *Discoverer {
	return &Discoverer{client: c, cache: map[string]string{}}
}

// Discover returns the environment's address, discovering and caching it on the
// first call. When there is no address yet it returns a message saying why, so
// the page can show the reason rather than a generic "starting".
func (d *Discoverer) Discover(ctx context.Context, env model.Env) (string, string) {
	d.mu.Lock()
	if base, ok := d.cache[env.ID]; ok {
		d.mu.Unlock()
		return base, ""
	}
	d.mu.Unlock()

	base, why := d.lookup(ctx, env)
	if base == "" {
		return "", why
	}
	d.mu.Lock()
	d.cache[env.ID] = base
	d.mu.Unlock()
	return base, ""
}

// Forget drops a cached address, so the next Discover re-reads the log. It is
// called when an environment turns out not to be at the address that was
// cached — a run that ended and was replaced has a new one.
func (d *Discoverer) Forget(envID string) {
	d.mu.Lock()
	delete(d.cache, envID)
	d.mu.Unlock()
}

// lookup reads a run's log and extracts the address from it, or says why it
// could not — a message for the page, not a verdict the reader has to guess
// from.
//
// Two things GitHub does make this unreliable, and both are said plainly rather
// than dressed up as "starting":
//
//   - The log archive is only served once a run has finished. While a run is in
//     progress the endpoint answers 404, so the address cannot be read during
//     the hours the environment is actually up.
//   - Even a finished run's log is a zip, and this reads the bytes as text. The
//     address is visible in the run's page and its summary; neither is a file
//     this service can fetch.
//
// So this is a best-effort path. A domain configured in LABS_DOMAIN_* is the one
// that actually works, and the message says so.
func (d *Discoverer) lookup(ctx context.Context, env model.Env) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	runs, err := d.client.Runs(ctx, env.Repo, env.Workflow, env.Ref, 5)
	if err != nil {
		return "", "could not list the workflow's runs: " + err.Error()
	}
	if len(runs) == 0 {
		return "", "no " + env.Workflow + " run has been started in " + env.Repo + " yet; set LABS_DOMAIN_" + env.ID + " to the environment's hostname to skip this"
	}

	sawRunning := false
	for _, run := range runs {
		if run.Status != "in_progress" && run.Status != "completed" {
			continue
		}
		sawRunning = sawRunning || run.Status == "in_progress"
		log, err := d.client.RunLogs(ctx, env.Repo, run.ID)
		if err != nil {
			// The common case: a running run has no downloadable log yet. Kept
			// short, because the reader's answer is the same whichever way this
			// failed — set the domain.
			return "", "the environment's address could not be read from its run log; set LABS_DOMAIN_" + env.ID + " to its hostname"
		}
		if base, ok := ExtractBaseURL(log, env.BasePath); ok {
			return base, ""
		}
	}
	if !sawRunning {
		return "", "the cluster is starting; its run has not begun yet"
	}
	return "", "the environment's address has not appeared in its run log; set LABS_DOMAIN_" + env.ID + " to its hostname"
}
