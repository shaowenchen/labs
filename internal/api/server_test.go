package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shaowenchen/labs/internal/config"
	"github.com/shaowenchen/labs/internal/driver"
	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/ratelimit"
	"github.com/shaowenchen/labs/internal/session"
	"github.com/shaowenchen/labs/internal/store"
)

// fakeSvc is a session service with none of the machinery behind it, so the
// route layer's statuses and JSON are exercised on their own.
type fakeSvc struct {
	provision func(req session.ProvisionRequest, ip string) (session.Result, error)
	sessions  map[string]model.Session
	released  []string
	readyAny  bool
	status    []session.EnvStatus
	live      []session.LiveLabs
	choices   []driver.Choice
}

func (f *fakeSvc) Provision(_ context.Context, req session.ProvisionRequest, ip string) (session.Result, error) {
	if f.provision == nil {
		return session.Result{}, store.ErrAtCapacity
	}
	return f.provision(req, ip)
}
func (f *fakeSvc) Get(id string) (model.Session, bool) { s, ok := f.sessions[id]; return s, ok }
func (f *fakeSvc) Release(_ context.Context, id string) error {
	f.released = append(f.released, id)
	return nil
}
func (f *fakeSvc) Status(context.Context) []session.EnvStatus             { return f.status }
func (f *fakeSvc) ReadyAny(context.Context) bool                          { return f.readyAny }
func (f *fakeSvc) Live(context.Context) []session.LiveLabs                { return f.live }
func (f *fakeSvc) ChoicesFor(context.Context, model.Kind) []driver.Choice { return f.choices }

func testConfig() config.Config {
	return config.Config{
		SessionTTL:       2 * time.Hour,
		MaxSessionsPerIP: 1,
		RateLimitCount:   5,
		RateLimitWindow:  time.Hour,
		Envs: []model.Env{{
			ID: "applab-1", Kind: model.KindApplab, Domain: "a.example.com", BasePath: "/applab",
			Slots: []string{"lab-01"}, Capacity: 1,
		}},
	}
}

func newTestServer(t *testing.T, svc SessionService, cfg config.Config, limit int) *Server {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	// fail() logs through the standard logger; point it at the discard handler
	// so an expected 503 in a test does not print.
	slog.SetDefault(discard)
	return New(Deps{
		Config:  cfg,
		Service: svc,
		Limiter: ratelimit.New(limit, time.Hour),
		Log:     discard,
	})
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// data unwraps the success envelope.
func data[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var body struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return body.Data
}

func TestCreateLabReturnsTheKeyOnce(t *testing.T) {
	svc := &fakeSvc{provision: func(req session.ProvisionRequest, ip string) (session.Result, error) {
		return session.Result{
			Session:    model.Session{ID: "sid", Kind: model.KindApplab, App: "lab-01", ExpiresAt: time.Now().Add(2 * time.Hour)},
			ConsoleURL: "https://a.example.com/applab",
			APIKey:     "user-key",
		}, nil
	}}
	s := newTestServer(t, svc, testConfig(), 5)

	w := do(t, s, "POST", "/api/v1/labs", `{"kind":"applab"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	got := data[map[string]any](t, w)
	if got["console_url"] != "https://a.example.com/applab" {
		t.Errorf("console_url = %v", got["console_url"])
	}
	if got["api_key"] != "user-key" {
		t.Errorf("api_key = %v, want the minted key", got["api_key"])
	}
	if got["session_id"] != "sid" {
		t.Errorf("session_id = %v", got["session_id"])
	}
}

func TestCreateLabWithoutABodyUsesTheDefaultKind(t *testing.T) {
	var seen model.Kind
	svc := &fakeSvc{provision: func(req session.ProvisionRequest, ip string) (session.Result, error) {
		seen = req.Kind
		return session.Result{Session: model.Session{ID: "s", Kind: req.Kind, ExpiresAt: time.Now()}, APIKey: "k"}, nil
	}}
	s := newTestServer(t, svc, testConfig(), 5)

	if w := do(t, s, "POST", "/api/v1/labs", ""); w.Code != http.StatusCreated {
		t.Fatalf("empty body: status = %d, body %s", w.Code, w.Body.String())
	}
	if seen != model.KindApplab {
		t.Errorf("default kind = %q, want applab", seen)
	}
}

func TestCreateLabRejectsAnUnknownKind(t *testing.T) {
	s := newTestServer(t, &fakeSvc{}, testConfig(), 5)
	w := do(t, s, "POST", "/api/v1/labs", `{"kind":"nope"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// No environment being up is the ordinary state during a reboot, and it must
// read as "come back", not as an internal error.
func TestCreateLabWithoutAReadyEnvIsRetryable(t *testing.T) {
	svc := &fakeSvc{provision: func(session.ProvisionRequest, string) (session.Result, error) {
		return session.Result{}, session.ErrNoReadyEnv
	}}
	cfg := testConfig()
	s := newTestServer(t, svc, cfg, 5)

	w := do(t, s, "POST", "/api/v1/labs", "{}")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a 503 for a starting environment should carry Retry-After")
	}
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if !body.Retryable {
		t.Error("a starting environment should be marked retryable")
	}
}

func TestCreateLabWhenFullIsServiceUnavailable(t *testing.T) {
	svc := &fakeSvc{provision: func(session.ProvisionRequest, string) (session.Result, error) {
		return session.Result{}, store.ErrAtCapacity
	}}
	s := newTestServer(t, svc, testConfig(), 5)
	if w := do(t, s, "POST", "/api/v1/labs", "{}"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestPerIPConcurrentLimitIsTooManyRequests(t *testing.T) {
	svc := &fakeSvc{provision: func(session.ProvisionRequest, string) (session.Result, error) {
		return session.Result{}, store.ErrIPLimit
	}}
	s := newTestServer(t, svc, testConfig(), 5)
	w := do(t, s, "POST", "/api/v1/labs", "{}")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("a 429 should carry Retry-After")
	}
}

func TestRateLimitStopsABurst(t *testing.T) {
	svc := &fakeSvc{provision: func(req session.ProvisionRequest, ip string) (session.Result, error) {
		return session.Result{Session: model.Session{ID: "s", ExpiresAt: time.Now()}, APIKey: "k"}, nil
	}}
	s := newTestServer(t, svc, testConfig(), 1) // one request per window

	if w := do(t, s, "POST", "/api/v1/labs", "{}"); w.Code != http.StatusCreated {
		t.Fatalf("first request: status = %d", w.Code)
	}
	w := do(t, s, "POST", "/api/v1/labs", "{}")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", w.Code)
	}
}

func TestGetLabNeverReturnsTheKey(t *testing.T) {
	svc := &fakeSvc{sessions: map[string]model.Session{
		"sid": {ID: "sid", Kind: model.KindApplab, ConsoleURL: "https://a.example.com/applab", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	s := newTestServer(t, svc, testConfig(), 5)

	w := do(t, s, "GET", "/api/v1/labs/sid", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "api_key") {
		t.Fatalf("a read-back carried a key: %s", w.Body.String())
	}
}

func TestGetUnknownLabIs404(t *testing.T) {
	s := newTestServer(t, &fakeSvc{sessions: map[string]model.Session{}}, testConfig(), 5)
	if w := do(t, s, "GET", "/api/v1/labs/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDeleteLabIsIdempotent(t *testing.T) {
	svc := &fakeSvc{sessions: map[string]model.Session{}}
	s := newTestServer(t, svc, testConfig(), 5)

	if w := do(t, s, "DELETE", "/api/v1/labs/sid", ""); w.Code != http.StatusOK {
		t.Fatalf("first delete: status = %d", w.Code)
	}
	if w := do(t, s, "DELETE", "/api/v1/labs/sid", ""); w.Code != http.StatusOK {
		t.Fatalf("second delete: status = %d, want 200", w.Code)
	}
}

func TestConfigIsAnonymousAndNamesTheKinds(t *testing.T) {
	svc := &fakeSvc{readyAny: true, status: []session.EnvStatus{{ID: "applab-1", Kind: model.KindApplab, Ready: true, Capacity: 1}}}
	s := newTestServer(t, svc, testConfig(), 5)

	w := do(t, s, "GET", "/api/v1/config", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got := data[map[string]any](t, w)
	kinds, _ := got["kinds"].([]any)
	if len(kinds) != 1 || kinds[0] != "applab" {
		t.Errorf("kinds = %v, want [applab]", got["kinds"])
	}
	if got["session_ttl"] != "2h0m0s" {
		t.Errorf("session_ttl = %v", got["session_ttl"])
	}
}

func TestReadyzReflectsTheEnvironments(t *testing.T) {
	down := newTestServer(t, &fakeSvc{readyAny: false}, testConfig(), 5)
	if w := do(t, down, "GET", "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with no environment up = %d, want 503", w.Code)
	}
	up := newTestServer(t, &fakeSvc{readyAny: true}, testConfig(), 5)
	if w := do(t, up, "GET", "/readyz", ""); w.Code != http.StatusOK {
		t.Fatalf("readyz with an environment up = %d, want 200", w.Code)
	}
}

func TestUnknownAPIPathIsJSONNotHTML(t *testing.T) {
	s := newTestServer(t, &fakeSvc{}, testConfig(), 5)
	w := do(t, s, "GET", "/api/v1/nope", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want JSON so a stale client does not parse HTML as success", ct)
	}
}

func TestRootServesTheConsole(t *testing.T) {
	s := newTestServer(t, &fakeSvc{}, testConfig(), 5)
	w := do(t, s, "GET", "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q, want HTML", ct)
	}
}

func TestWrongMethodIsRefused(t *testing.T) {
	s := newTestServer(t, &fakeSvc{}, testConfig(), 5)
	if w := do(t, s, "PUT", "/api/v1/labs", "{}"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

// A deployment with an incomplete configuration must not answer a create with a
// bare "no environment is available" — it must say what to set, in the response.
func TestCreateLabWhenUnconfiguredReturnsTheProblems(t *testing.T) {
	cfg := testConfig()
	cfg.Problems = []string{"GITHUB_TOKEN is not set", "LABS_REPOS is not set"}
	s := newTestServer(t, &fakeSvc{}, cfg, 5)

	w := do(t, s, "POST", "/api/v1/labs", "{}")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Problems) != 2 {
		t.Fatalf("problems = %v, want the two configuration problems", body.Problems)
	}
}

// /api/v1/config reports whether the deployment is configured, so a client can
// tell "not set up yet" from "set up but nothing is running".
func TestConfigReportsConfiguredState(t *testing.T) {
	cfg := testConfig()
	cfg.Problems = []string{"GITHUB_TOKEN is not set"}
	s := newTestServer(t, &fakeSvc{}, cfg, 5)

	got := data[map[string]any](t, do(t, s, "GET", "/api/v1/config", ""))
	if got["configured"] != false {
		t.Errorf("configured = %v, want false", got["configured"])
	}
	if probs, _ := got["problems"].([]any); len(probs) != 1 {
		t.Errorf("problems = %v, want one", got["problems"])
	}
}

// readyz carries the same verdict, so a probe sees it too.
func TestReadyzReportsUnconfigured(t *testing.T) {
	cfg := testConfig()
	cfg.Problems = []string{"GITHUB_TOKEN is not set"}
	s := newTestServer(t, &fakeSvc{readyAny: true}, cfg, 5)

	w := do(t, s, "GET", "/readyz", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz on an unconfigured deployment = %d, want 503 even with an environment up", w.Code)
	}
}

// An environment up but refusing the key is reported as such: it is a real
// verdict about the environment, distinct from a cluster that is still coming
// up, even though the page has nothing to offer for it but the fact.
func TestConfigReportsUnauthorizedEnvironment(t *testing.T) {
	svc := &fakeSvc{status: []session.EnvStatus{{ID: "APPLAB", Kind: model.KindApplab, Unauthorized: true, ConsoleURL: "https://a.example.com/applab"}}}
	s := newTestServer(t, svc, testConfig(), 5)

	got := data[map[string]any](t, do(t, s, "GET", "/api/v1/config", ""))
	envs, _ := got["environments"].([]any)
	if len(envs) != 1 {
		t.Fatalf("environments = %v", got["environments"])
	}
	e, _ := envs[0].(map[string]any)
	if e["unauthorized"] != true {
		t.Errorf("unauthorized = %v, want true", e["unauthorized"])
	}
}
