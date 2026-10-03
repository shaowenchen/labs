// Package sandboxlab drives a sandboxlab environment.
//
// The shape it uses is sandboxlab's sandbox: POST /api/v1/sandboxes creates a
// disposable workspace with its own two-hour clock, and the deployment's reaper
// deletes it when the clock runs out — so a session here expires on the
// environment's own schedule even if this service is not running, which is one
// thing applab cannot do.
//
// The credential is the deployment's single key. sandboxlab removed per-user
// keys upstream, so there is one key that reaches every sandbox, and this
// driver hands it to the caller with a warning that says so. It is the honest
// description of what the deployment supports; a per-user key, if it returns,
// would change this method and nothing else.
package sandboxlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shaowenchen/labs/internal/driver"
	"github.com/shaowenchen/labs/internal/model"
)

// sharedKeyWarning is delivered alongside a session, because the key is not
// scoped to the session and a caller should know that before they use it.
const sharedKeyWarning = "this lab uses the environment's shared key, which can reach every sandbox in the deployment"

// Driver implements driver.Driver for sandboxlab environments.
type Driver struct {
	hc  *http.Client
	log *slog.Logger

	// ttl is how long a sandbox lives. It is the session length, so the
	// environment's own reaper expires a sandbox at the same moment this service
	// would — one clock instead of two.
	ttl time.Duration
}

// New returns a sandboxlab driver. ttl is the session length the environment is
// asked to give each sandbox.
func New(ttl time.Duration, log *slog.Logger) *Driver {
	return &Driver{
		hc:  &http.Client{Timeout: 30 * time.Second},
		log: log,
		ttl: ttl,
	}
}

// Kind reports the kind this driver handles.
func (d *Driver) Kind() model.Kind { return model.KindSandboxlab }

// Ready probes GET /api/v1/config, which needs no key, and confirms with
// /healthz. The config also carries the address the console is served at, which
// is what a caller is sent to.
func (d *Driver) Ready(ctx context.Context, env model.Env) (driver.Ready, error) {
	var cfg struct {
		APIVersion string `json:"apiVersion"`
		PublicURL  string `json:"publicURL"`
		BasePath   string `json:"basePath"`
	}
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/config", "", nil, &cfg, false); err != nil {
		return driver.Ready{Message: "config: " + err.Error()}, nil
	}
	if cfg.APIVersion == "" {
		return driver.Ready{Message: "the config response carried no apiVersion"}, nil
	}

	var health json.RawMessage
	if err := d.call(ctx, env, http.MethodGet, "/healthz", "", nil, &health, false); err != nil {
		return driver.Ready{Message: "healthz: " + err.Error()}, nil
	}

	// The console address comes from what the environment reports, so a
	// deployment that is served somewhere other than where it was reached still
	// hands out the right link.
	console := env.BaseURL()
	if cfg.PublicURL != "" {
		console = strings.TrimRight(cfg.PublicURL, "/") + cfg.BasePath
	}

	// A keyed call: it is what says whether the key this service holds is the
	// one the environment accepts, rather than merely that it is up.
	var sandboxes json.RawMessage
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/sandboxes", env.APIKey, nil, &sandboxes, false); err != nil {
		if isUnauthorized(err) {
			return driver.Ready{
				ConsoleURL:   console,
				Unauthorized: true,
				Message:      "the environment is up but did not accept the key set for it",
			}, nil
		}
		return driver.Ready{Message: "sandboxes: " + err.Error()}, nil
	}
	return driver.Ready{Ready: true, ConsoleURL: console}, nil
}

// Provision creates a sandbox and returns where to reach it.
//
// The address is the sandbox's own data-plane URL with the key in it, so the
// link opens straight into the sandbox rather than dropping the caller on a
// console to find it. When the environment reports no endpoint — no public URL,
// or the data plane off — the console address is delivered instead and the key
// is pasted there.
func (d *Driver) Provision(ctx context.Context, env model.Env, req driver.ProvisionRequest) (driver.Provisioned, error) {
	template, err := d.templateFor(ctx, env)
	if err != nil {
		return driver.Provisioned{}, err
	}

	body := map[string]any{
		"template": template,
		"name":     sandboxName(req.SessionID),
		"ttl":      d.ttl.String(),
	}

	var sb struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		Endpoints []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"endpoints"`
	}
	if err := d.call(ctx, env, http.MethodPost, "/api/v1/sandboxes", env.APIKey, body, &sb, false); err != nil {
		return driver.Provisioned{}, fmt.Errorf("sandboxlab: create a sandbox: %w", err)
	}
	if sb.ID == "" {
		return driver.Provisioned{}, fmt.Errorf("sandboxlab: creating a sandbox returned no id")
	}

	console := baseConsole(env)
	for _, e := range sb.Endpoints {
		if e.URL != "" {
			console = withKey(e.URL, env.APIKey)
			break
		}
	}

	d.log.Info("minted a sandboxlab session", "env", env.ID, "sandbox", sb.ID, "template", template)
	return driver.Provisioned{
		ConsoleURL: console,
		APIKey:     env.APIKey,
		SandboxID:  sb.ID,
		Warning:    sharedKeyWarning,
	}, nil
}

// Release deletes the session's sandbox. It is idempotent.
//
// The environment's own reaper deletes a sandbox when its TTL runs out, so this
// is the early-exit path rather than the only one; a sandbox already gone is
// the outcome this wanted.
func (d *Driver) Release(ctx context.Context, env model.Env, sess model.Session) error {
	if sess.SandboxID == "" {
		return nil
	}
	path := "/api/v1/sandboxes/" + url.PathEscape(sess.SandboxID)
	if err := d.call(ctx, env, http.MethodDelete, path, env.APIKey, nil, nil, true); err != nil && !isNotFound(err) {
		d.log.Warn("could not delete a released session's sandbox", "env", env.ID, "sandbox", sess.SandboxID, "error", err)
		return fmt.Errorf("sandboxlab: delete sandbox %q: %w", sess.SandboxID, err)
	}
	d.log.Info("released a sandboxlab session", "env", env.ID, "sandbox", sess.SandboxID)
	return nil
}

// EnsureSlot is a no-op: sandboxlab has no slots to pre-create. It is here
// because the interface has it for the kinds that do.
func (d *Driver) EnsureSlot(context.Context, model.Env, string) error { return nil }

// Reconcile deletes any of this service's sandboxes that no live session holds.
//
// It only ever deletes a sandbox whose id begins with the prefix this service
// names its sandboxes with, so a sandbox someone created by hand — with the
// shared key, or in the console — is never touched. That scoping is the whole
// safety of the pass: the shared key sees every sandbox, and deleting by
// absence would otherwise delete a stranger's work.
func (d *Driver) Reconcile(ctx context.Context, env model.Env, live []model.Session) error {
	held := map[string]bool{}
	for _, s := range live {
		if s.SandboxID != "" {
			held[s.SandboxID] = true
		}
	}

	var out struct {
		Sandboxes []struct {
			ID string `json:"id"`
		} `json:"sandboxes"`
	}
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/sandboxes", env.APIKey, nil, &out, false); err != nil {
		return fmt.Errorf("sandboxlab: list sandboxes: %w", err)
	}

	var firstErr error
	for _, sb := range out.Sandboxes {
		if held[sb.ID] || !strings.HasPrefix(sb.ID, sandboxPrefix) {
			continue
		}
		if err := d.Release(ctx, env, model.Session{SandboxID: sb.ID}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// templateFor is the template a session creates. A configured one is used as
// given; otherwise the catalog's first template is, so a deployment that names
// no template still gets a working sandbox from whatever it has.
func (d *Driver) templateFor(ctx context.Context, env model.Env) (string, error) {
	if env.Template != "" {
		return env.Template, nil
	}
	var out struct {
		Templates []struct {
			ID string `json:"id"`
		} `json:"templates"`
	}
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/catalog", env.APIKey, nil, &out, false); err != nil {
		return "", fmt.Errorf("sandboxlab: read the catalog: %w", err)
	}
	if len(out.Templates) == 0 {
		return "", fmt.Errorf("sandboxlab: the environment offers no templates to create a sandbox from")
	}
	return out.Templates[0].ID, nil
}

// ── naming and addresses ────────────────────────────────────────────────────

// sandboxPrefix is what this service names its sandboxes, so Reconcile can tell
// its own from anyone else's.
const sandboxPrefix = "lab-"

// sandboxName is a cluster-safe name for a session's sandbox: sandbox ids are
// lowercase alphanumerics and '-', and a session id is already lowercase hex.
func sandboxName(sessionID string) string {
	short := sessionID
	if len(short) > 8 {
		short = short[:8]
	}
	return sandboxPrefix + short
}

// baseConsole is the environment's console address, the fallback when a sandbox
// reports no endpoint of its own.
func baseConsole(env model.Env) string { return env.BaseURL() }

// withKey puts the key in a URL as the query parameter sandboxlab reads for
// browser access, so the link opens the sandbox directly.
func withKey(rawURL, key string) string {
	if rawURL == "" || key == "" {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "key=" + url.QueryEscape(key)
}

// ── the HTTP call ───────────────────────────────────────────────────────────

// call performs one request against the environment. sandboxlab answers with
// the payload directly — it does not wrap responses in an envelope the way
// applab does — so the body is decoded as-is.
func (d *Driver) call(ctx context.Context, env model.Env, method, path, key string, body, out any, notFoundOK bool) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, env.BaseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		// sandboxlab reads X-Sandbox-Key, or a bearer token; both are sent so
		// the call works whichever a deployment has been configured for.
		req.Header.Set("X-Sandbox-Key", key)
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusNotFound && notFoundOK {
		return errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(raw))}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// httpError is a non-2xx answer, carrying enough for a caller to classify it.
type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

var errNotFound = &httpError{status: http.StatusNotFound, body: "not found"}

func isNotFound(err error) bool {
	he, ok := err.(*httpError)
	return ok && he.status == http.StatusNotFound
}

// isUnauthorized reports whether a call was refused for its key rather than for
// anything else.
func isUnauthorized(err error) bool {
	he, ok := err.(*httpError)
	if !ok {
		return false
	}
	return he.status == http.StatusUnauthorized || he.status == http.StatusForbidden
}
