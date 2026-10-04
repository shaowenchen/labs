package applab

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shaowenchen/labs/internal/driver"
	"github.com/shaowenchen/labs/internal/model"
)

// fakeAppLab is a scripted AppLab: it records the calls made against it and
// answers the shapes the driver expects.
type fakeAppLab struct {
	mu        sync.Mutex
	calls     []string
	key       string
	refuseKey bool
	conflict  bool             // POST /apps answers 409, as it does for an existing app
	health    string           // the health route to serve; "/healthz" exercises the fallback
	apps      []map[string]any // what GET /apps answers
}

func (f *fakeAppLab) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeAppLab) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAppLab) server(base string) *httptest.Server {
	mux := http.NewServeMux()
	// applab serves its health route at /health, not /healthz — the two are
	// different paths and only one of them exists. The default fake serves the
	// real one, so a driver that probed the wrong path alone would fail here;
	// f.health moves it to /healthz for the fallback test.
	healthRoute := f.health
	if healthRoute == "" {
		healthRoute = "/health"
	}
	mux.HandleFunc("GET "+healthRoute, func(w http.ResponseWriter, r *http.Request) {
		f.record("health")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		f.record("config")
		writeData(w, http.StatusOK, map[string]any{"api_version": "v1", "version": "test"})
	})
	// The keyed probe Ready makes to tell "usable" from "key refused". The
	// 401/403 behaviour is set by the test through f.refuseKey.
	mux.HandleFunc("GET /api/v1/apps", func(w http.ResponseWriter, r *http.Request) {
		f.record("apps")
		if f.refuseKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid key"}`))
			return
		}
		apps := f.apps
		if apps == nil {
			apps = []map[string]any{}
		}
		writeData(w, http.StatusOK, apps)
	})
	mux.HandleFunc("POST /api/v1/apps", func(w http.ResponseWriter, r *http.Request) {
		f.record("create")
		if f.conflict {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"app already exists","retryable":false}`))
			return
		}
		writeData(w, http.StatusCreated, map[string]any{"id": "lab-01", "app_key": "created-key"})
	})
	mux.HandleFunc("POST /api/v1/apps/{app}/key/rotate", func(w http.ResponseWriter, r *http.Request) {
		f.record("rotate:" + r.PathValue("app"))
		f.mu.Lock()
		f.key = "user-key-" + r.PathValue("app")
		key := f.key
		f.mu.Unlock()
		writeData(w, http.StatusOK, map[string]any{"app_id": r.PathValue("app"), "key": key})
	})
	mux.HandleFunc("POST /api/v1/apps/{app}/stop", func(w http.ResponseWriter, r *http.Request) {
		f.record("stop:" + r.PathValue("app"))
		writeData(w, http.StatusOK, map[string]any{"stopped": r.PathValue("app")})
	})
	return httptest.NewServer(http.StripPrefix(base, mux))
}

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func testEnv(server string) model.Env {
	return model.Env{
		ID:       "applab-1",
		Kind:     model.KindApplab,
		Domain:   "applab-1.example.com",
		BasePath: "/applab",
		Slots:    []string{"lab-01", "lab-02"},
		Capacity: 2,
		APIKey:   "admin-key",
	}
}

// envWithURL points the environment at the fake's address by overriding the
// scheme and host, since the driver builds URLs from Domain and BasePath.
func envWithURL(server, basePath string) model.Env {
	e := testEnv(server)
	e.Scheme = "http"
	e.Domain = strings.TrimPrefix(server, "http://")
	e.BasePath = basePath
	return e
}

func newDriver(t *testing.T) *Driver {
	t.Helper()
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestReadyParsesConfig(t *testing.T) {
	f := &fakeAppLab{}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	r, err := newDriver(t).Ready(context.Background(), env)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !r.Ready {
		t.Fatalf("Ready = false: %s", r.Message)
	}
	if r.ConsoleURL != env.BaseURL() {
		t.Errorf("ConsoleURL = %q, want %q", r.ConsoleURL, env.BaseURL())
	}
}

func TestReadyFallsBackToHealthz(t *testing.T) {
	// A deployment that answers at /healthz rather than /health must still be
	// read as up: the probe tries /health first and falls back, so the route the
	// environment actually serves is not mistaken for a dead one.
	f := &fakeAppLab{health: "/healthz"}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	r, err := newDriver(t).Ready(context.Background(), env)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !r.Ready {
		t.Fatalf("Ready = false for a server whose health route is /healthz: %s", r.Message)
	}
}

func TestReadyIsFalseWhenDown(t *testing.T) {
	// A server that is not there: the ordinary state between one run and the
	// next, and it must read as "not ready", not as an error.
	env := envWithURL("http://127.0.0.1:1", "/applab")

	r, err := newDriver(t).Ready(context.Background(), env)
	if err != nil {
		t.Fatalf("Ready returned an error for a down environment: %v", err)
	}
	if r.Ready {
		t.Fatal("Ready = true for an environment that is not there")
	}
}

func TestProvisionRotatesAndReturnsTheKey(t *testing.T) {
	f := &fakeAppLab{}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")
	env.Slots = []string{"lab-01"}

	got, err := newDriver(t).Provision(context.Background(), env, req("sess-1", "lab-01"))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got.APIKey != "user-key-lab-01" {
		t.Errorf("APIKey = %q, want the rotated key", got.APIKey)
	}
	if got.App != "lab-01" {
		t.Errorf("App = %q, want lab-01", got.App)
	}
	if got.ConsoleURL != env.BaseURL() {
		t.Errorf("ConsoleURL = %q, want %q", got.ConsoleURL, env.BaseURL())
	}
}

func TestEnsureSlotTreatsConflictAsSuccess(t *testing.T) {
	f := &fakeAppLab{conflict: true}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	if err := newDriver(t).EnsureSlot(context.Background(), env, "lab-01"); err != nil {
		t.Fatalf("EnsureSlot should treat an existing app as success: %v", err)
	}
}

// Release must rotate the key away before it stops the app. If it stopped first
// and the stop failed, the caller would be left holding a working key — the
// order is the point, so it is asserted.
func TestReleaseRotatesBeforeStopping(t *testing.T) {
	f := &fakeAppLab{}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	err := newDriver(t).Release(context.Background(), env, model.Session{App: "lab-01"})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	order := f.order()
	var rotateAt, stopAt = -1, -1
	for i, c := range order {
		switch {
		case strings.HasPrefix(c, "rotate:"):
			rotateAt = i
		case strings.HasPrefix(c, "stop:"):
			stopAt = i
		}
	}
	if rotateAt < 0 || stopAt < 0 {
		t.Fatalf("expected a rotate and a stop, got %v", order)
	}
	if rotateAt > stopAt {
		t.Fatalf("rotate happened after stop: %v", order)
	}
}

func TestReleaseWithNoAppIsANoOp(t *testing.T) {
	f := &fakeAppLab{}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	if err := newDriver(t).Release(context.Background(), env, model.Session{}); err != nil {
		t.Fatalf("Release with no app: %v", err)
	}
	if len(f.order()) != 0 {
		t.Fatalf("Release with no app made calls: %v", f.order())
	}
}

func TestReconcileOnlyTouchesConfiguredSlots(t *testing.T) {
	f := &fakeAppLab{}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")
	env.Slots = []string{"lab-01", "lab-02"}

	// lab-01 is live; lab-02 is not and must be released.
	live := []model.Session{{App: "lab-01"}}
	if err := newDriver(t).Reconcile(context.Background(), env, live); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	calls := f.order()
	if contains(calls, "rotate:lab-01") {
		t.Errorf("reconcile touched a live slot: %v", calls)
	}
	if !contains(calls, "rotate:lab-02") {
		t.Errorf("reconcile did not release the dead slot: %v", calls)
	}
}

func req(sessionID, app string) driver.ProvisionRequest {
	return driver.ProvisionRequest{SessionID: sessionID, App: app}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// An environment that is up but refuses the key must report Unauthorized, not a
// generic not-ready: the answer is a key to enter, not a wait.
func TestReadyReportsUnauthorized(t *testing.T) {
	f := &fakeAppLab{refuseKey: true}
	srv := f.server("/applab")
	defer srv.Close()

	r, err := newDriver(t).Ready(context.Background(), envWithURL(srv.URL, "/applab"))
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if r.Ready {
		t.Fatal("Ready = true for an environment that refused the key")
	}
	if !r.Unauthorized {
		t.Fatal("Ready should report Unauthorized when the key is refused")
	}
	if r.ConsoleURL == "" {
		t.Error("an unauthorized environment still has an address")
	}
}

// A long error body is cut to a status-line length, because it is shown to
// whoever is watching the environment come up, not to a debugger — and an edge
// proxy's error page is hundreds of bytes of JSON.
func TestLongErrorBodyIsShortened(t *testing.T) {
	long := strings.Repeat("x", 900)
	if got := briefBody([]byte(long)); len(got) > 220 {
		t.Errorf("briefBody kept %d characters, want a short line", len(got))
	}
	// A real, short error is left exactly as it came.
	if got := briefBody([]byte(`  {"error":"invalid key"}  `)); got != `{"error":"invalid key"}` {
		t.Errorf("briefBody(%q) = %q, want it unchanged and trimmed", `{"error":"invalid key"}`, got)
	}
}

// Live reports the slots that are actually running, and only those: an app
// still in "created" is a pre-made slot with nothing behind it, which is what
// an idle slot is, so it is not a running lab and must not be counted.
func TestLiveSkipsCreatedSlots(t *testing.T) {
	f := &fakeAppLab{apps: []map[string]any{
		{"id": "lab-01", "status": "created", "created_at": "2026-10-04T13:00:00Z"},
		{"id": "lab-02", "status": "running", "created_at": "2026-10-04T13:05:00Z"},
		{"id": "someone-else", "status": "running", "created_at": "2026-10-04T13:06:00Z"},
	}}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")
	env.Slots = []string{"lab-01", "lab-02"}

	got, err := newDriver(t).Live(context.Background(), env)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(got) != 1 || got[0].ID != "lab-02" {
		t.Fatalf("Live = %+v, want only the running slot lab-02", got)
	}
	if got[0].State != "running" || got[0].CreatedAt.IsZero() {
		t.Errorf("Live[0] = %+v, want its state and creation time", got[0])
	}
	// An applab app has no expiry of its own; the caller applies the session clock.
	if !got[0].ExpiresAt.IsZero() {
		t.Errorf("applab Live should carry no expiry, got %s", got[0].ExpiresAt)
	}
}
