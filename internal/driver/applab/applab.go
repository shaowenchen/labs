// Package applab drives an AppLab environment.
//
// The shape it exploits is AppLab's per-app key. An app key reaches exactly one
// app and nothing else, and rotating it invalidates the previous value at once —
// so a session's credential is a freshly rotated key for the slot it holds, and
// ending the session is another rotation. That is a real per-session credential,
// which is why applab is the kind that fits "hand over a link" with nothing
// shared between callers.
//
// The deployment's own admin key is what performs the rotations. It is held in
// the environment's config and never reaches a caller.
package applab

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

// Driver implements driver.Driver for AppLab environments.
type Driver struct {
	hc  *http.Client
	log *slog.Logger
}

// New returns an applab driver.
func New(log *slog.Logger) *Driver {
	return &Driver{
		hc:  &http.Client{Timeout: 30 * time.Second},
		log: log,
	}
}

// Kind reports the kind this driver handles.
func (d *Driver) Kind() model.Kind { return model.KindApplab }

// Ready probes GET /api/v1/config, which needs no key, and then a call that
// does — to tell "up and usable" from "up but the key is wrong".
//
// The distinction is the whole point of the second call: an environment that
// answers its config but refuses the admin key is reachable and only needs the
// right key, which the page can ask for, rather than a wait that never ends.
// A non-200, a timeout or a body that is not AppLab's config otherwise means
// "not up yet", the ordinary gap between runs.
func (d *Driver) Ready(ctx context.Context, env model.Env) (driver.Ready, error) {
	var cfg struct {
		APIVersion string `json:"api_version"`
		Version    string `json:"version"`
	}
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/config", "", nil, &cfg, false); err != nil {
		return driver.Ready{Message: "config: " + err.Error()}, nil
	}
	if cfg.APIVersion == "" {
		return driver.Ready{Message: "the config response carried no api_version"}, nil
	}

	// healthz is cheap and is what a deployment that is up but not ready fails.
	var health json.RawMessage
	if err := d.call(ctx, env, http.MethodGet, "/healthz", "", nil, &health, false); err != nil {
		return driver.Ready{Message: "healthz: " + err.Error()}, nil
	}

	// A keyed call: it is what says whether the key this service holds is the
	// one the environment accepts.
	var apps json.RawMessage
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/apps", env.APIKey, nil, &apps, false); err != nil {
		if isUnauthorized(err) {
			return driver.Ready{
				ConsoleURL:   env.BaseURL(),
				Unauthorized: true,
				Message:      "the environment is up but did not accept the key set for it",
			}, nil
		}
		return driver.Ready{Message: "apps: " + err.Error()}, nil
	}
	return driver.Ready{Ready: true, ConsoleURL: env.BaseURL()}, nil
}

// EnsureSlot makes sure an app record exists for the slot, so a later Provision
// has something to rotate a key for.
//
// The app is created inert — no auto-deploy — because it is a slot, not a
// deployment: it becomes real when a session pushes something into it. A create
// that answers "already exists" is success, which is what makes this safe to run
// on every warm-up.
func (d *Driver) EnsureSlot(ctx context.Context, env model.Env, app string) error {
	if app == "" {
		return nil
	}
	body := map[string]any{"id": app, "name": app, "auto_deploy": false}
	err := d.call(ctx, env, http.MethodPost, "/api/v1/apps", env.APIKey, body, nil, true)
	if err != nil && isAlreadyExists(err) {
		return nil
	}
	return err
}

// Provision rotates the slot's key and returns the fresh value as the caller's
// credential.
//
// Rotating is a create when the app has no key yet, so a slot that lost its key
// recovers here rather than failing. The address delivered is the environment's
// console, where the caller pastes the key.
func (d *Driver) Provision(ctx context.Context, env model.Env, req driver.ProvisionRequest) (driver.Provisioned, error) {
	app := req.App
	if app == "" {
		return driver.Provisioned{}, fmt.Errorf("applab: a session needs an app slot, but none was given")
	}
	if err := d.EnsureSlot(ctx, env, app); err != nil {
		return driver.Provisioned{}, fmt.Errorf("applab: ensure slot %q: %w", app, err)
	}

	var out struct {
		Key string `json:"key"`
	}
	path := "/api/v1/apps/" + url.PathEscape(app) + "/key/rotate"
	if err := d.call(ctx, env, http.MethodPost, path, env.APIKey, nil, &out, false); err != nil {
		return driver.Provisioned{}, fmt.Errorf("applab: rotate key for %q: %w", app, err)
	}
	if out.Key == "" {
		return driver.Provisioned{}, fmt.Errorf("applab: rotating the key for %q returned no key", app)
	}

	d.log.Info("minted an applab session", "env", env.ID, "app", app)
	return driver.Provisioned{
		ConsoleURL: env.BaseURL(),
		APIKey:     out.Key,
		App:        app,
	}, nil
}

// Release ends a session: the key is rotated away first, then the app is
// stopped.
//
// The order is the point. Rotating immediately invalidates the credential the
// caller holds, so even if stopping the app then fails — the cluster is
// unreachable, the run is being torn down — no one is left holding a working
// key. The reverse order would leave a live credential behind exactly when the
// teardown was going badly, which is the case that matters.
func (d *Driver) Release(ctx context.Context, env model.Env, sess model.Session) error {
	app := sess.App
	if app == "" {
		return nil
	}

	rotatePath := "/api/v1/apps/" + url.PathEscape(app) + "/key/rotate"
	if err := d.call(ctx, env, http.MethodPost, rotatePath, env.APIKey, nil, nil, true); err != nil && !isNotFound(err) {
		// Reported, not returned yet: the credential may still be live, which is
		// the serious half, so this is a warning worth acting on.
		d.log.Warn("could not rotate away a released session's key", "env", env.ID, "app", app, "error", err)
		return fmt.Errorf("applab: rotate key for %q: %w", app, err)
	}

	stopPath := "/api/v1/apps/" + url.PathEscape(app) + "/stop"
	if err := d.call(ctx, env, http.MethodPost, stopPath, env.APIKey, nil, nil, true); err != nil && !isNotFound(err) {
		// The credential is already dead, so this is a cleanup failure rather
		// than a security one. Downgraded to a warning.
		d.log.Warn("could not stop a released session's app", "env", env.ID, "app", app, "error", err)
	}

	d.log.Info("released an applab session", "env", env.ID, "app", app)
	return nil
}

// Reconcile rotates and stops every slot app that no live session holds.
//
// It considers only the environment's configured slots: an app a person created
// by hand is not this service's to clean up, and touching it would make the
// reconciliation the thing that causes the outage.
func (d *Driver) Reconcile(ctx context.Context, env model.Env, live []model.Session) error {
	held := map[string]bool{}
	for _, s := range live {
		if s.App != "" {
			held[s.App] = true
		}
	}
	var firstErr error
	for _, app := range env.Slots {
		if held[app] {
			continue
		}
		if err := d.Release(ctx, env, model.Session{App: app}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ── the HTTP call ───────────────────────────────────────────────────────────

// call performs one request against the environment and decodes the "data"
// envelope AppLab answers in.
//
// notFoundOK makes a 404 a nil error, for the callers that treat "already gone"
// as the outcome they wanted.
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
	if out == nil {
		return nil
	}
	return decodeData(raw, out)
}

// decodeData unwraps AppLab's {"data": ...} envelope.
func decodeData(raw []byte, out any) error {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	return json.Unmarshal(env.Data, out)
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

func isAlreadyExists(err error) bool {
	he, ok := err.(*httpError)
	if !ok {
		return false
	}
	return he.status == http.StatusConflict || strings.Contains(strings.ToLower(he.body), "exists")
}

// isUnauthorized reports whether a call was refused for its key rather than for
// anything else: 401, or 403 missing a credential.
func isUnauthorized(err error) bool {
	he, ok := err.(*httpError)
	if !ok {
		return false
	}
	return he.status == http.StatusUnauthorized || he.status == http.StatusForbidden
}
