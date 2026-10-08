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
	"time"

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
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["id"].(string)
		writeData(w, http.StatusCreated, map[string]any{"id": id, "app_key": "created-key"})
	})
	mux.HandleFunc("POST /api/v1/apps/{app}/key/rotate", func(w http.ResponseWriter, r *http.Request) {
		f.record("rotate:" + r.PathValue("app"))
		f.mu.Lock()
		f.key = "user-key-" + r.PathValue("app")
		key := f.key
		f.mu.Unlock()
		writeData(w, http.StatusOK, map[string]any{"app_id": r.PathValue("app"), "key": key})
	})
	mux.HandleFunc("DELETE /api/v1/apps/{app}", func(w http.ResponseWriter, r *http.Request) {
		f.record("delete:" + r.PathValue("app"))
		writeData(w, http.StatusOK, map[string]any{"id": r.PathValue("app"), "deleted": true})
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
	// The reason is the state, not the transport error behind it. A visitor
	// reading the page is handed "not answering yet"; the dial error that says
	// why is the log's, and putting it here is how an edge proxy's 530 ends up
	// on a page as though it were the service's own fault.
	if r.Message != driver.NotAnswering {
		t.Errorf("Message = %q, want %q", r.Message, driver.NotAnswering)
	}
	if strings.Contains(r.Message, "dial") || strings.Contains(r.Message, "http://") {
		t.Errorf("Message carries the probe's own error, not a description: %q", r.Message)
	}
}

// A tunnel in front of an environment with no run answers 530 with Cloudflare's
// error page. That is the exact response this page used to show verbatim —
// "config: HTTP 530: Error 1033: Cloudflare Tunnel error" — which names an edge
// proxy's failure to someone who only wants to know whether the lab is ready.
func TestReadyDoesNotSurfaceATunnelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(530)
		_, _ = w.Write([]byte(`{"title":"Error 1033: Cloudflare Tunnel error"}`))
	}))
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	r, err := newDriver(t).Ready(context.Background(), env)
	if err != nil {
		t.Fatalf("Ready returned an error for a 530: %v", err)
	}
	if r.Ready {
		t.Fatal("Ready = true for an environment answering 530")
	}
	for _, leak := range []string{"530", "1033", "Cloudflare", "Tunnel", "config:"} {
		if strings.Contains(r.Message, leak) {
			t.Errorf("Message leaks the proxy error (%q): %q", leak, r.Message)
		}
	}
}

func TestProvisionCreatesTheAppAndReturnsItsKey(t *testing.T) {
	f := &fakeAppLab{}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	got, err := newDriver(t).Provision(context.Background(), env, req("sess-1"))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// The name is minted from the session id, so it is `lab-` plus a prefix of
	// that id and nothing is reused between sessions.
	want := "lab-" + "sess-1"
	if got.App != want {
		t.Errorf("App = %q, want %q", got.App, want)
	}
	if got.APIKey != "user-key-"+want {
		t.Errorf("APIKey = %q, want the rotated key", got.APIKey)
	}
	if got.ConsoleURL != env.BaseURL() {
		t.Errorf("ConsoleURL = %q, want %q", got.ConsoleURL, env.BaseURL())
	}
	// The app must be created before its key is rotated: a rotate on an app that
	// does not exist has nothing to rotate.
	order := f.order()
	if !contains(order, "create") {
		t.Fatalf("Provision did not create the app: %v", order)
	}
}

// A create that answers "already exists" is an error, not success: with a name
// minted per session the only way to hit one is a collision with an app that
// belongs to someone else.
func TestProvisionRefusesAnAppNameCollision(t *testing.T) {
	f := &fakeAppLab{conflict: true}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	_, err := newDriver(t).Provision(context.Background(), env, req("sess-1"))
	if err == nil {
		t.Fatal("Provision should fail when the app name already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to name the collision", err)
	}
}

// Release must rotate the key away before it deletes the app. If it deleted
// first and the delete failed, the caller would be left holding a working key —
// the order is the point, so it is asserted.
func TestReleaseRotatesBeforeDeleting(t *testing.T) {
	f := &fakeAppLab{}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	err := newDriver(t).Release(context.Background(), env, model.Session{App: "lab-abc123"})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	order := f.order()
	var rotateAt, deleteAt = -1, -1
	for i, c := range order {
		switch {
		case strings.HasPrefix(c, "rotate:"):
			rotateAt = i
		case strings.HasPrefix(c, "delete:"):
			deleteAt = i
		}
	}
	if rotateAt < 0 || deleteAt < 0 {
		t.Fatalf("expected a rotate and a delete, got %v", order)
	}
	if rotateAt > deleteAt {
		t.Fatalf("rotate happened after delete: %v", order)
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

// Reconcile cleans up by prefix and by what the environment lists, not by a
// configured set of names: an app the service made that nobody holds is
// released, one it did not make is left alone.
func TestReconcileOnlyTouchesAppsOfOurs(t *testing.T) {
	f := &fakeAppLab{apps: []map[string]any{
		{"id": "lab-live", "status": "created", "created_at": time.Now().Add(-time.Hour).Format(time.RFC3339)},
		{"id": "lab-orphan", "status": "created", "created_at": time.Now().Add(-time.Hour).Format(time.RFC3339)},
		{"id": "someone-elses", "status": "created", "created_at": time.Now().Add(-time.Hour).Format(time.RFC3339)},
	}}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	live := []model.Session{{App: "lab-live"}}
	if err := newDriver(t).Reconcile(context.Background(), env, live); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	calls := f.order()
	if contains(calls, "rotate:lab-live") {
		t.Errorf("reconcile touched a live app: %v", calls)
	}
	if contains(calls, "rotate:someone-elses") {
		t.Errorf("reconcile touched an app it did not make: %v", calls)
	}
	if !contains(calls, "rotate:lab-orphan") {
		t.Errorf("reconcile did not release an orphan: %v", calls)
	}
}

// An app younger than the grace period is spared: Provision creates it and the
// session that holds it is recorded a moment later, so inside that window it is
// an app nobody holds — and releasing it would take a lab away from its caller.
func TestReconcileSparesAnAppInsideTheGraceWindow(t *testing.T) {
	f := &fakeAppLab{apps: []map[string]any{
		{"id": "lab-justmade", "status": "created", "created_at": time.Now().Format(time.RFC3339)},
	}}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	if err := newDriver(t).Reconcile(context.Background(), env, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if contains(f.order(), "rotate:lab-justmade") {
		t.Errorf("reconcile released an app that was just created: %v", f.order())
	}
}

func req(sessionID string) driver.ProvisionRequest {
	return driver.ProvisionRequest{SessionID: sessionID}
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
	// A JSON error body is reduced to the message inside it, so a proxy's page
	// reads as its one useful line.
	if got := briefBody([]byte(`{"error":"invalid key"}`)); got != "invalid key" {
		t.Errorf("briefBody = %q, want the error field", got)
	}
	// Something that is not JSON is left as it came.
	if got := briefBody([]byte("  error code: 1033  ")); got != "error code: 1033" {
		t.Errorf("briefBody = %q, want it unchanged and trimmed", got)
	}
}

// Live reports every app of this service's, whatever the app's state, and is
// scoped by the name prefix: an app someone made by hand with the shared key is
// not one of ours and must not be counted.
func TestLiveReportsEveryAppOfOurs(t *testing.T) {
	f := &fakeAppLab{apps: []map[string]any{
		{"id": "lab-9f2c1a3b", "status": "created", "created_at": "2026-10-04T13:00:00Z"},
		{"id": "lab-4d7e8f01", "status": "running", "created_at": "2026-10-04T13:05:00Z"},
		{"id": "someone-else", "status": "running", "created_at": "2026-10-04T13:06:00Z"},
	}}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	got, err := newDriver(t).Live(context.Background(), env)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Live = %+v, want both of this service's apps and not the hand-made one", got)
	}
	if got[0].ID != "lab-9f2c1a3b" || got[0].State != "created" {
		t.Errorf("Live[0] = %+v, want its state", got[0])
	}
	if got[1].ID != "lab-4d7e8f01" || got[1].State != "running" || got[1].CreatedAt.IsZero() {
		t.Errorf("Live[1] = %+v, want its state and creation time", got[1])
	}
	// An applab app has no expiry of its own; the caller applies the session clock.
	if !got[0].ExpiresAt.IsZero() || !got[1].ExpiresAt.IsZero() {
		t.Errorf("applab Live should carry no expiry, got %+v", got)
	}
}

// Provision and Live must agree about an app that was just handed out. Provision
// creates the app inert — auto_deploy is off, so applab reports it as "created"
// until the caller pushes something into it — and Live is what the page draws
// its rows from. Filtering "created" out of Live therefore made a lab vanish
// from the list the instant it was created and only reappear if the caller
// deployed into the app, which is exactly backwards: the moment a caller has a
// lab is when they are looking for it.
func TestProvisionedAppIsVisibleToLive(t *testing.T) {
	// What applab reports for an app that was created and nothing more, which is
	// what Provision leaves behind.
	f := &fakeAppLab{apps: []map[string]any{
		{"id": "lab-sess-1", "status": "created", "created_at": "2026-10-04T13:00:00Z"},
	}}
	srv := f.server("/applab")
	defer srv.Close()
	env := envWithURL(srv.URL, "/applab")

	d := newDriver(t)
	if _, err := d.Provision(context.Background(), env, req("sess-1")); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	got, err := d.Live(context.Background(), env)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if len(got) != 1 || got[0].ID != "lab-sess-1" {
		t.Fatalf("a just-provisioned app is not in the list: %+v", got)
	}
}
