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
// first call.
func (d *Discoverer) Discover(ctx context.Context, env model.Env) (string, bool) {
	d.mu.Lock()
	if base, ok := d.cache[env.ID]; ok {
		d.mu.Unlock()
		return base, true
	}
	d.mu.Unlock()

	base, ok := d.lookup(ctx, env)
	if !ok {
		return "", false
	}
	d.mu.Lock()
	d.cache[env.ID] = base
	d.mu.Unlock()
	return base, true
}

// Forget drops a cached address, so the next Discover re-reads the log. It is
// called when an environment turns out not to be at the address that was
// cached — a run that ended and was replaced has a new one.
func (d *Discoverer) Forget(envID string) {
	d.mu.Lock()
	delete(d.cache, envID)
	d.mu.Unlock()
}

// lookup reads the newest run's log and extracts the address from it.
func (d *Discoverer) lookup(ctx context.Context, env model.Env) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	runs, err := d.client.Runs(ctx, env.Repo, env.Workflow, env.Ref, 5)
	if err != nil {
		return "", false
	}
	for _, run := range runs {
		// Only a run that is up can have printed a live address.
		if run.Status != "in_progress" {
			continue
		}
		log, err := d.client.RunLogs(ctx, env.Repo, run.ID)
		if err != nil {
			continue
		}
		if base, ok := ExtractBaseURL(log, env.BasePath); ok {
			return base, true
		}
	}
	return "", false
}
