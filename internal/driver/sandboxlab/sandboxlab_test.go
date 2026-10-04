package sandboxlab

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

// fakeSandboxlab is a scripted sandboxlab: it records the calls made against it
// and answers the bare-JSON shapes the driver expects.
type fakeSandboxlab struct {
	mu        sync.Mutex
	calls     []string
	sandboxes []map[string]any
}

func (f *fakeSandboxlab) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeSandboxlab) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSandboxlab) server(base string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		f.note("config")
		writeJSON(w, http.StatusOK, map[string]any{
			"apiVersion": "v1", "publicURL": "https://sb.example.com", "basePath": "/sandbox",
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		f.note("healthz")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/catalog", func(w http.ResponseWriter, r *http.Request) {
		f.note("catalog")
		writeJSON(w, http.StatusOK, map[string]any{"templates": []map[string]any{{"id": "all-in-one"}, {"id": "python"}}})
	})
	mux.HandleFunc("POST /api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		f.note("create")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id, _ := body["name"].(string)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": id, "template": body["template"], "state": "Running",
			"endpoints": []map[string]any{{"name": "desktop", "port": 3000, "url": "https://sb.example.com/sandbox/sandbox/" + id + "/3000/"}},
		})
	})
	mux.HandleFunc("GET /api/v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		f.note("list")
		f.mu.Lock()
		list := append([]map[string]any(nil), f.sandboxes...)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"sandboxes": list, "count": len(list)})
	})
	mux.HandleFunc("DELETE /api/v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.note("delete:" + r.PathValue("id"))
		writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id")})
	})
	return httptest.NewServer(http.StripPrefix(base, mux))
}

func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(obj)
}

func envWithURL(server, basePath string) model.Env {
	return model.Env{
		ID:       "SANDBOXLAB",
		Kind:     model.KindSandboxlab,
		Scheme:   "http",
		Domain:   strings.TrimPrefix(server, "http://"),
		BasePath: basePath,
		Capacity: 8,
		APIKey:   "shared-key",
	}
}

func newDriver(t *testing.T) *Driver {
	t.Helper()
	return New(2*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestReadyReportsTheConsoleFromConfig(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()

	r, err := newDriver(t).Ready(context.Background(), envWithURL(srv.URL, "/sandbox"))
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !r.Ready {
		t.Fatalf("Ready = false: %s", r.Message)
	}
	// The console address comes from what the environment reported, not from the
	// address it was reached at.
	if r.ConsoleURL != "https://sb.example.com/sandbox" {
		t.Errorf("ConsoleURL = %q, want the environment's own public URL", r.ConsoleURL)
	}
}

func TestReadyIsFalseWhenDown(t *testing.T) {
	r, err := newDriver(t).Ready(context.Background(), envWithURL("http://127.0.0.1:1", "/sandbox"))
	if err != nil {
		t.Fatalf("Ready returned an error for a down environment: %v", err)
	}
	if r.Ready {
		t.Fatal("Ready = true for an environment that is not there")
	}
}

func TestProvisionCreatesASandboxAndKeysTheLink(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()
	env := envWithURL(srv.URL, "/sandbox")

	got, err := newDriver(t).Provision(context.Background(), env, driver.ProvisionRequest{SessionID: "abcdef1234567890"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got.SandboxID != "lab-abcdef12" {
		t.Errorf("SandboxID = %q, want lab-abcdef12", got.SandboxID)
	}
	if got.APIKey != "shared-key" {
		t.Errorf("APIKey = %q, want the deployment key", got.APIKey)
	}
	if !strings.Contains(got.ConsoleURL, "key=shared-key") {
		t.Errorf("ConsoleURL = %q, want the key in the link", got.ConsoleURL)
	}
	if got.Warning == "" {
		t.Error("a shared key should come with a warning")
	}
}

// With no template configured, the catalog's first is used.
func TestProvisionUsesTheFirstTemplateWhenNoneIsSet(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()

	if _, err := newDriver(t).Provision(context.Background(), envWithURL(srv.URL, "/sandbox"), driver.ProvisionRequest{SessionID: "aaaaaaaa"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !contains(f.order(), "catalog") {
		t.Errorf("expected the catalog to be read, got %v", f.order())
	}
}

func TestReleaseDeletesTheSandbox(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()

	if err := newDriver(t).Release(context.Background(), envWithURL(srv.URL, "/sandbox"), model.Session{SandboxID: "lab-1"}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !contains(f.order(), "delete:lab-1") {
		t.Errorf("expected the sandbox to be deleted, got %v", f.order())
	}
}

func TestReleaseWithNoSandboxIsANoOp(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()

	if err := newDriver(t).Release(context.Background(), envWithURL(srv.URL, "/sandbox"), model.Session{}); err != nil {
		t.Fatalf("Release with no sandbox: %v", err)
	}
	if len(f.order()) != 0 {
		t.Fatalf("Release with no sandbox made calls: %v", f.order())
	}
}

// Reconcile deletes this service's sandboxes that no live session holds, and
// nothing else — a sandbox someone created by hand is never touched.
func TestReconcileOnlyDeletesOursThatAreNotLive(t *testing.T) {
	f := &fakeSandboxlab{sandboxes: []map[string]any{
		{"id": "lab-live"},      // ours, live
		{"id": "lab-orphan"},    // ours, not live
		{"id": "someone-elses"}, // not ours
	}}
	srv := f.server("/sandbox")
	defer srv.Close()

	live := []model.Session{{SandboxID: "lab-live"}}
	if err := newDriver(t).Reconcile(context.Background(), envWithURL(srv.URL, "/sandbox"), live); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	calls := f.order()
	if contains(calls, "delete:lab-live") {
		t.Errorf("reconcile deleted a live sandbox: %v", calls)
	}
	if contains(calls, "delete:someone-elses") {
		t.Errorf("reconcile deleted a sandbox it did not create: %v", calls)
	}
	if !contains(calls, "delete:lab-orphan") {
		t.Errorf("reconcile did not delete an orphan: %v", calls)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestLongErrorBodyIsShortened(t *testing.T) {
	long := strings.Repeat("x", 900)
	if got := briefBody([]byte(long)); len(got) > 220 {
		t.Errorf("briefBody kept %d characters, want a short line", len(got))
	}
	if got := briefBody([]byte(`  {"error":"invalid key"}  `)); got != `{"error":"invalid key"}` {
		t.Errorf("briefBody trimmed a short body to %q", got)
	}
}
