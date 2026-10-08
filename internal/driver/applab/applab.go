// Package applab drives an AppLab environment.
//
// The shape it exploits is AppLab's per-app key. An app key reaches exactly one
// app and nothing else, and rotating it invalidates the previous value at once —
// so a session's credential is a freshly rotated key for the app it owns, and
// ending the session is another rotation. That is a real per-session credential,
// which is why applab is the kind that fits "hand over a link" with nothing
// shared between callers.
//
// Each session's app is named after the session — `lab-` and a prefix of its id
// — so an app id is never reused and never comes from configuration.
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

	// now is the clock Reconcile compares an app's age against. It is a field so
	// a test can place an app's creation in the past without waiting.
	now func() time.Time
}

// New returns an applab driver.
func New(log *slog.Logger) *Driver {
	return &Driver{
		hc:  &http.Client{Timeout: 30 * time.Second},
		log: log,
		now: time.Now,
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
		return d.notAnswering(env, "config", err), nil
	}
	if cfg.APIVersion == "" {
		// Answered, but not with the shape this service knows. That is a
		// deployment problem — the wrong thing is serving the address — and the
		// field it is missing is this service's business, not the reader's.
		d.log.Warn("an environment answered with no api_version", "env", env.ID, "route", "config")
		return driver.Ready{Message: driver.WrongDeployment}, nil
	}

	// The health route is cheap and is what a deployment that is up but not
	// ready fails. applab serves it at /health (a kubelet probe path, matched as
	// a suffix so the base path is included); /healthz is tried second, because
	// a build that answers there should not be read as down. The first probe
	// that answers 200 wins, and a body that will not decode is not a failure
	// here — a 200 is the answer this is asking for.
	if err := d.healthy(ctx, env); err != nil {
		return d.notAnswering(env, "health", err), nil
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
		return d.notAnswering(env, "apps", err), nil
	}
	return driver.Ready{Ready: true, ConsoleURL: env.BaseURL()}, nil
}

// notAnswering is the verdict for a probe that got no usable answer: the error
// goes to the log, where it is worth reading, and a sentence about the state
// goes back to the caller, who is not in a position to act on the error's
// details. Which route failed is in the log entry.
func (d *Driver) notAnswering(env model.Env, route string, err error) driver.Ready {
	d.log.Warn("a readiness probe did not answer", "env", env.ID, "route", route, "error", err)
	return driver.Ready{Message: driver.NotAnswering}
}

// healthy probes the environment's health route without a key.
//
// applab serves it at /health — the path its own kubelet probe uses, matched as
// a suffix so it sits under the base path too. /healthz is tried as well, so a
// deployment that answers there (or a build that moved the route) is not read
// as down; the first 200 ends the search. Only a status that is not a 200, or a
// refused connection, is a failure — the body is not decoded, because a 200 is
// already the answer.
func (d *Driver) healthy(ctx context.Context, env model.Env) error {
	var firstErr error
	for _, route := range []string{"/health", "/healthz"} {
		err := d.call(ctx, env, http.MethodGet, route, "", nil, nil, false)
		if err == nil {
			return nil
		}
		// A 401/403 means the route answered but wanted a key, which is still up.
		if isUnauthorized(err) {
			return nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// appPrefix is what this service names its apps, so Reconcile can tell its own
// from anyone else's.
const appPrefix = "lab-"

// reconcileGrace is how long an app this service named is left alone before
// Reconcile will delete it as an orphan. It has to be longer than the gap
// between Provision creating the app and the session that holds it being
// recorded — one HTTP round trip — because inside that gap the app is real and
// unheld, and deleting it would take a lab away from the caller it is being made
// for. Minutes rather than seconds, since nobody is waiting on the cleanup.
const reconcileGrace = 2 * time.Minute

// appName is a cluster-safe app id for a session's app. applab requires the id
// to be lowercase letters, digits and '-', which the session id already is.
//
// Twelve hex characters rather than sandboxlab's eight, and deliberately: a
// collision here is not the same failure. sandboxlab's name goes to a create
// that fails on a duplicate, so a collision there costs one session. This name
// is the handle an app is then managed by, and a collision would mean creating
// an app that already belongs to a live session. Twelve characters is 48 bits,
// which puts a collision out of reach for a service that holds a handful of
// sessions at a time.
func appName(sessionID string) string {
	short := sessionID
	if len(short) > 12 {
		short = short[:12]
	}
	return appPrefix + short
}

// Provision mints a session's app and returns a fresh key for it as the caller's
// credential.
//
// The app id is minted here rather than configured: it is per session and never
// reused, so nothing accumulates under one name and two sessions can never be
// confused for one another. The app is created inert — no auto-deploy — because
// it is a place to put an application, not a deployment: it becomes real when
// the session pushes something into it. A create that answers "already exists"
// is an error rather than success, because with a fresh id per session the only
// way to get one is a collision with an app that belongs to someone else.
//
// The address delivered is the environment's console, where the caller pastes
// the key.
func (d *Driver) Provision(ctx context.Context, env model.Env, req driver.ProvisionRequest) (driver.Provisioned, error) {
	if req.SessionID == "" {
		return driver.Provisioned{}, fmt.Errorf("applab: a session needs an id to name its app after, but none was given")
	}
	app := appName(req.SessionID)

	body := map[string]any{"id": app, "name": app, "auto_deploy": false}
	if err := d.call(ctx, env, http.MethodPost, "/api/v1/apps", env.APIKey, body, nil, false); err != nil {
		if isAlreadyExists(err) {
			return driver.Provisioned{}, fmt.Errorf("applab: an app named %q already exists, which a fresh name should never hit", app)
		}
		return driver.Provisioned{}, fmt.Errorf("applab: create app %q: %w", app, err)
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
// deleted with everything applab recorded for it.
//
// The order is the point. Rotating immediately invalidates the credential the
// caller holds, so even if the delete then fails — the cluster is unreachable,
// the run is being torn down — no one is left holding a working key. The reverse
// order would leave a live credential behind exactly when the teardown was going
// badly, which is the case that matters.
//
// The delete is what keeps the app list from growing without bound: the id is
// minted per session and never reused, so an app left behind is one no future
// session will ever take over. It removes the app's cluster objects, its source
// and its credential in one call.
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

	deletePath := "/api/v1/apps/" + url.PathEscape(app)
	if err := d.call(ctx, env, http.MethodDelete, deletePath, env.APIKey, nil, nil, true); err != nil && !isNotFound(err) {
		// The credential is already dead, so this is a cleanup failure rather
		// than a security one. Downgraded to a warning.
		d.log.Warn("could not delete a released session's app", "env", env.ID, "app", app, "error", err)
	}

	d.log.Info("released an applab session", "env", env.ID, "app", app)
	return nil
}

// Live lists the apps this service has in the environment, in use and idle
// alike. It reads the state from the environment, so it reflects what is there
// rather than what this service last recorded.
//
// It is scoped by the app-name prefix: an app carrying this service's prefix was
// made by it, and one that does not belongs to someone else and is not reported.
// The same prefix is what Reconcile cleans up by, so the two agree on what is
// ours.
//
// Every app listed is one of this service's and counts as in use, whatever the
// app's own state says: an app recorded but not yet deployed — the state right
// after a lab is handed out, before anything is pushed into it — is still an app
// this service made and must account for.
//
// No key is read or returned: the key is a separate route (/key), and a listing
// that carried it would hand out a credential per row. ExpiresAt is left zero —
// an applab app has no expiry of its own, so the caller applies the session's
// clock (created plus the TTL).
func (d *Driver) Live(ctx context.Context, env model.Env) ([]driver.Live, error) {
	// GET /api/v1/apps answers {"data":[ ... ]} — the data value is the array
	// itself, not an object wrapping one.
	var apps []struct {
		ID        string    `json:"id"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/apps", env.APIKey, nil, &apps, false); err != nil {
		return nil, fmt.Errorf("applab: list apps: %w", err)
	}

	live := make([]driver.Live, 0, len(apps))
	for _, a := range apps {
		if !strings.HasPrefix(a.ID, appPrefix) {
			continue
		}
		live = append(live, driver.Live{ID: a.ID, State: a.Status, CreatedAt: a.CreatedAt})
	}
	return live, nil
}

// Choices is nil: applab hands out an app of this service's own naming and there
// is nothing about it to pick. A lab there is the app plus a key, and the app is
// this service's to name rather than the caller's to choose.
func (d *Driver) Choices(context.Context, model.Env) ([]driver.Choice, error) { return nil, nil }

// Reconcile deletes every app of this service's that no live session holds.
//
// It lists the environment rather than iterating a configured set: the names are
// minted per session, so the only way to know what exists is to ask. Only apps
// carrying this service's prefix are touched — an app a person created by hand
// is not this service's to clean up, and touching it would make the
// reconciliation the thing that causes the outage.
func (d *Driver) Reconcile(ctx context.Context, env model.Env, live []model.Session) error {
	held := map[string]bool{}
	for _, s := range live {
		if s.App != "" {
			held[s.App] = true
		}
	}

	var apps []struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := d.call(ctx, env, http.MethodGet, "/api/v1/apps", env.APIKey, nil, &apps, false); err != nil {
		return fmt.Errorf("applab: list apps: %w", err)
	}

	var firstErr error
	for _, a := range apps {
		if held[a.ID] || !strings.HasPrefix(a.ID, appPrefix) {
			continue
		}
		// An app younger than the grace period is one this process may be
		// mid-way through handing out: Provision creates it and the session that
		// will hold it is recorded a moment later, so between the two it is an
		// app nobody holds. Releasing it there would delete a lab that is on its
		// way to a caller — and, unlike the key rotation this used to be, the
		// delete takes the source with it. Waiting costs an orphan a few minutes;
		// not waiting costs a caller their work. An app left by a restart is
		// hours old and is still cleaned up.
		if !a.CreatedAt.IsZero() && d.now().Sub(a.CreatedAt) < reconcileGrace {
			continue
		}
		if err := d.Release(ctx, env, model.Session{App: a.ID}); err != nil && firstErr == nil {
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
		return &httpError{status: resp.StatusCode, body: briefBody(raw)}
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

// briefBody is an error body short enough to sit on a status line, and worded
// the way the far end worded it.
//
// It is not a debugging aid but a message for whoever is watching an
// environment come up. An edge proxy answers with a page of JSON whose useful
// part is one field — a Cloudflare 1033 is "Cloudflare Tunnel error" buried in a
// few hundred bytes — so a JSON object is reduced to its title or error, and
// anything else is cut to a line. The full body is still what the caller got;
// this is only how it is worded.
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
