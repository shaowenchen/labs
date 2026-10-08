package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaowenchen/labs/internal/config"
	"github.com/shaowenchen/labs/internal/driver"
	"github.com/shaowenchen/labs/internal/driver/applab"
	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/ratelimit"
	"github.com/shaowenchen/labs/internal/session"
	"github.com/shaowenchen/labs/internal/store"
)

// This is the one test that wires the real pieces together — the real manager,
// the real applab driver, the real store, the real HTTP layer — against a fake
// AppLab that speaks the same API. The unit tests prove each part; this proves
// the whole path a caller takes, from POST /api/v1/labs to a key that a request
// to AppLab actually carries.

type fakeAppLab struct {
	mu       sync.Mutex
	order    []string
	keys     map[string]string
	conflict bool
}

func (f *fakeAppLab) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, s)
}

func (f *fakeAppLab) handler(base string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		f.note("config")
		writeEnvelope(w, http.StatusOK, map[string]any{"api_version": "v1", "version": "fake"})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		f.note("healthz")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// The keyed probe Ready makes; it answers OK so the environment reads ready.
	mux.HandleFunc("GET /api/v1/apps", func(w http.ResponseWriter, r *http.Request) {
		f.note("apps")
		writeEnvelope(w, http.StatusOK, []any{})
	})
	mux.HandleFunc("POST /api/v1/apps", func(w http.ResponseWriter, r *http.Request) {
		f.note("create")
		if f.conflict {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"app already exists"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeEnvelope(w, http.StatusCreated, map[string]any{"id": body["id"]})
	})
	mux.HandleFunc("POST /api/v1/apps/{app}/key/rotate", func(w http.ResponseWriter, r *http.Request) {
		app := r.PathValue("app")
		f.note("rotate:" + app)
		f.mu.Lock()
		if f.keys == nil {
			f.keys = map[string]string{}
		}
		f.keys[app] = "key-" + app + "-v2"
		key := f.keys[app]
		f.mu.Unlock()
		writeEnvelope(w, http.StatusOK, map[string]any{"app_id": app, "key": key})
	})
	mux.HandleFunc("DELETE /api/v1/apps/{app}", func(w http.ResponseWriter, r *http.Request) {
		f.note("delete:" + r.PathValue("app"))
		writeEnvelope(w, http.StatusOK, map[string]any{"id": r.PathValue("app"), "deleted": true})
	})
	return http.StripPrefix(base, mux)
}

func writeEnvelope(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// newE2E builds the whole stack pointed at a fake AppLab.
func newE2E(t *testing.T) (*Server, *fakeAppLab) {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(discard)

	f := &fakeAppLab{}
	srv := httptest.NewServer(f.handler("/applab"))
	t.Cleanup(srv.Close)

	env := model.Env{
		ID:       "applab-1",
		Kind:     model.KindApplab,
		Scheme:   "http",
		Domain:   strings.TrimPrefix(srv.URL, "http://"),
		BasePath: "/applab",
		Capacity: 2,
		APIKey:   "admin-key",
	}
	cfg := config.Config{
		SessionTTL:       2 * time.Hour,
		MaxSessionsPerIP: 2,
		RateLimitCount:   100,
		RateLimitWindow:  time.Hour,
		Envs:             []model.Env{env},
	}
	st := store.New()
	drivers := map[model.Kind]driver.Driver{model.KindApplab: applab.New(discard)}
	manager := session.New(cfg, st, drivers, discard)

	return New(Deps{Config: cfg, Service: manager, Limiter: ratelimit.New(100, time.Hour), Log: discard}), f
}

func TestEndToEndDeliverAndExpire(t *testing.T) {
	s, f := newE2E(t)

	// 1. A caller asks for a lab and gets a console and a key.
	res := do(t, s, "POST", "/api/v1/labs", `{"kind":"applab"}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("create lab: status = %d, body %s", res.Code, res.Body.String())
	}
	lab := data[map[string]any](t, res)
	key, _ := lab["api_key"].(string)
	if key == "" {
		t.Fatalf("no key was delivered: %s", res.Body.String())
	}
	if !strings.HasPrefix(key, "key-lab-") {
		t.Errorf("key %q does not look like a rotated app key", key)
	}

	// 2. The key was minted by a rotation against AppLab, which is what makes it
	//    a real per-session credential rather than a shared one.
	if !hasPrefix(f.order, "rotate:") {
		t.Errorf("provisioning did not rotate a key: %v", f.order)
	}

	// 3. Reading the lab back must not hand the key out again.
	read := do(t, s, "GET", "/api/v1/labs/"+lab["session_id"].(string), "")
	if read.Code != http.StatusOK {
		t.Fatalf("read lab: status = %d", read.Code)
	}
	if strings.Contains(read.Body.String(), key) {
		t.Fatalf("reading a lab back leaked its key: %s", read.Body.String())
	}

	// 4. A second caller gets its own app and a different key: the app id is
	//    minted per session, so two sessions can never share one.
	res2 := do(t, s, "POST", "/api/v1/labs", "{}")
	lab2 := data[map[string]any](t, res2)
	if lab2["api_key"] == key {
		t.Fatal("two callers were given the same key")
	}
	if lab2["app"] == lab["app"] {
		t.Fatalf("two callers were given the same app %v", lab["app"])
	}

	// 5. Ending the first lab rotates its key away and deletes its app, in that
	//    order — the key must be dead before the app is torn down.
	before := len(f.order)
	if del := do(t, s, "DELETE", "/api/v1/labs/"+lab["session_id"].(string), ""); del.Code != http.StatusOK {
		t.Fatalf("delete lab: status = %d", del.Code)
	}
	tail := f.order[before:]
	rotateAt, deleteAt := -1, -1
	for i, c := range tail {
		if c == "rotate:"+lab["app"].(string) {
			rotateAt = i
		}
		if c == "delete:"+lab["app"].(string) {
			deleteAt = i
		}
	}
	if rotateAt < 0 || deleteAt < 0 {
		t.Fatalf("releasing did not rotate then delete: %v", tail)
	}
	if rotateAt > deleteAt {
		t.Fatalf("the app was deleted before its key was rotated away: %v", tail)
	}

	// 6. The freed capacity comes back, and the app name does not: a name is
	//    minted per session and never handed out twice.
	res3 := do(t, s, "POST", "/api/v1/labs", "{}")
	lab3 := data[map[string]any](t, res3)
	if lab3["app"] == lab["app"] {
		t.Errorf("the released app name %q was handed out again", lab["app"])
	}
}

// With the environment not answering, a create is a retryable 503, not a 500.
func TestEndToEndHandlesADownEnvironment(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(discard)

	cfg := config.Config{
		SessionTTL: 2 * time.Hour, RateLimitCount: 100, RateLimitWindow: time.Hour, MaxSessionsPerIP: 1,
		Envs: []model.Env{{
			ID: "applab-1", Kind: model.KindApplab, Scheme: "http",
			Domain: "127.0.0.1:1", BasePath: "/applab", Capacity: 1, APIKey: "k",
		}},
	}
	st := store.New()
	manager := session.New(cfg, st, map[model.Kind]driver.Driver{model.KindApplab: applab.New(discard)}, discard)
	s := New(Deps{Config: cfg, Service: manager, Limiter: ratelimit.New(100, time.Hour), Log: discard})

	res := do(t, s, "POST", "/api/v1/labs", "{}")
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("with the environment down: status = %d, want 503", res.Code)
	}
	if res.Header().Get("Retry-After") == "" {
		t.Error("a 503 for a down environment should carry Retry-After")
	}
	if w := do(t, s, "GET", "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz with the environment down = %d, want 503", w.Code)
	}
}

func hasPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
