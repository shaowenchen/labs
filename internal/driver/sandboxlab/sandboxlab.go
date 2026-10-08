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
		return d.notAnswering(env, "config", err), nil
	}
	if cfg.APIVersion == "" {
		// Answered, but not with the shape this service knows. That is a
		// deployment problem — the wrong thing is serving the address — and the
		// field it is missing is this service's business, not the reader's.
		d.log.Warn("an environment answered with no apiVersion", "env", env.ID, "route", "config")
		return driver.Ready{Message: driver.WrongDeployment}, nil
	}

	var health json.RawMessage
	if err := d.call(ctx, env, http.MethodGet, "/healthz", "", nil, &health, false); err != nil {
		return d.notAnswering(env, "healthz", err), nil
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
		return d.notAnswering(env, "sandboxes", err), nil
	}
	return driver.Ready{Ready: true, ConsoleURL: console}, nil
}

// notAnswering is the verdict for a probe that got no usable answer: the error
// goes to the log, where it is worth reading, and a sentence about the state
// goes back to the caller. See driver.NotAnswering for why the two are split.
func (d *Driver) notAnswering(env model.Env, route string, err error) driver.Ready {
	d.log.Warn("a readiness probe did not answer", "env", env.ID, "route", route, "error", err)
	return driver.Ready{Message: driver.NotAnswering}
}

// Provision creates a sandbox and returns where to reach it.
//
// The template is the caller's, when they named one; otherwise it is the
// environment's configured default, and failing that the catalog's first entry.
// The one actually used is echoed back, because the caller's request may have
// named nothing and the session has to record what ran.
//
// The address is the sandbox's own data-plane URL with the key in it, so the
// link opens straight into the sandbox rather than dropping the caller on a
// console to find it. When the environment reports no endpoint — no public URL,
// or the data plane off — the console address is delivered instead and the key
// is pasted there.
func (d *Driver) Provision(ctx context.Context, env model.Env, req driver.ProvisionRequest) (driver.Provisioned, error) {
	template, err := d.templateFor(ctx, env, req.Template)
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
		Template:   template,
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

// Live lists the sandboxes this service is running. It is scoped to the prefix
// this service names its sandboxes with, so a sandbox someone made by hand with
// the shared key is not reported as one of ours.
//
// Unlike an applab app, a sandboxlab sandbox carries its own clock: CreatedAt
// and ExpiresAt are the sandbox's, not a session record's, so they are right
// even for a sandbox this process did not create.
func (d *Driver) Live(ctx context.Context, env model.Env) ([]driver.Live, error) {
	var out struct {
		Sandboxes []struct {
			ID        string    `json:"id"`
			State     string    `json:"state"`
			CreatedAt time.Time `json:"createdAt"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"sandboxes"`
	}
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/sandboxes", env.APIKey, nil, &out, false); err != nil {
		return nil, fmt.Errorf("sandboxlab: list sandboxes: %w", err)
	}

	live := make([]driver.Live, 0, len(out.Sandboxes))
	for _, sb := range out.Sandboxes {
		if !strings.HasPrefix(sb.ID, sandboxPrefix) {
			continue
		}
		live = append(live, driver.Live{ID: sb.ID, State: sb.State, CreatedAt: sb.CreatedAt, ExpiresAt: sb.ExpiresAt})
	}
	return live, nil
}

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

// Choices lists the catalog's templates — what a caller picks a sandbox from.
// sandboxlab's catalog is the whole list of what the deployment can make, which
// is exactly the choice being offered, so it is passed through rather than
// filtered: a template this service has no opinion about is still one a caller
// may want.
func (d *Driver) Choices(ctx context.Context, env model.Env) ([]driver.Choice, error) {
	var out struct {
		Templates []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"templates"`
	}
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/catalog", env.APIKey, nil, &out, false); err != nil {
		return nil, fmt.Errorf("sandboxlab: read the catalog: %w", err)
	}
	choices := make([]driver.Choice, 0, len(out.Templates))
	for _, t := range out.Templates {
		if t.ID == "" {
			continue
		}
		choices = append(choices, driver.Choice{ID: t.ID, Title: t.Title, Description: t.Description})
	}
	return choices, nil
}

// templateFor is the template a session creates: the caller's, when they named
// one and the environment offers it; otherwise a configured default; otherwise
// the catalog's first, so a deployment that names nothing still gets a working
// sandbox from whatever it has.
//
// A requested template that the environment does not offer is refused rather
// than substituted. Falling back would hand someone a sandbox they did not ask
// for while telling them they got the one they did, and the refusal names the
// ones that do exist so the next request can succeed.
func (d *Driver) templateFor(ctx context.Context, env model.Env, requested string) (string, error) {
	if requested != "" {
		choices, err := d.Choices(ctx, env)
		if err != nil {
			return "", err
		}
		for _, c := range choices {
			if c.ID == requested {
				return requested, nil
			}
		}
		return "", fmt.Errorf("sandboxlab: no template named %q: this environment offers %s",
			requested, strings.Join(choiceIDs(choices), ", "))
	}
	if env.Template != "" {
		return env.Template, nil
	}
	choices, err := d.Choices(ctx, env)
	if err != nil {
		return "", err
	}
	if len(choices) == 0 {
		return "", fmt.Errorf("sandboxlab: the environment offers no templates to create a sandbox from")
	}
	return choices[0].ID, nil
}

// choiceIDs is the available template ids, for an error that says what to try
// instead of only what failed.
func choiceIDs(choices []driver.Choice) []string {
	out := make([]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, c.ID)
	}
	return out
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
		return &httpError{status: resp.StatusCode, body: briefBody(raw)}
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

// briefBody is an error body short enough to sit on a status line, and worded
// the way the far end worded it.
//
// It is not a debugging aid but a message for whoever is watching an
// environment come up. An edge proxy answers with a page of JSON whose useful
// part is one field, so a JSON object is reduced to its title or error, and
// anything else is cut to a line. A short body is left exactly as it came —
// sandboxlab's own answers are one line ("invalid key").
func briefBody(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	var obj struct {
		Title   string `json:"title"`
		Error   string `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal([]byte(s), &obj) == nil {
		for _, part := range []string{obj.Title, obj.Error, obj.Message, obj.Detail} {
			if part = strings.TrimSpace(part); part != "" {
				return clip(part)
			}
		}
	}
	return clip(s)
}

func clip(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
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
