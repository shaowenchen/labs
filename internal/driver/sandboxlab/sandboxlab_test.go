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

// A template the caller named is the one created, and it comes back so the
// session can record what ran.
func TestProvisionUsesTheRequestedTemplate(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()

	got, err := newDriver(t).Provision(context.Background(), envWithURL(srv.URL, "/sandbox"),
		driver.ProvisionRequest{SessionID: "abcdef1234567890", Template: "python"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got.Template != "python" {
		t.Errorf("Template = %q, want the one asked for", got.Template)
	}
}

// A template the environment does not offer is refused rather than quietly
// substituted — handing someone a sandbox they did not ask for while telling
// them they got the one they did is worse than a failed request.
func TestProvisionRefusesATemplateTheEnvironmentDoesNotOffer(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()

	_, err := newDriver(t).Provision(context.Background(), envWithURL(srv.URL, "/sandbox"),
		driver.ProvisionRequest{SessionID: "abcdef1234567890", Template: "nope"})
	if err == nil {
		t.Fatal("Provision accepted a template the environment does not offer")
	}
	// The refusal names what does exist, so the next request can succeed.
	if !strings.Contains(err.Error(), "python") || !strings.Contains(err.Error(), "all-in-one") {
		t.Errorf("the refusal should name the available templates, got %v", err)
	}
	// Nothing was created for a request that could not be satisfied.
	if contains(f.order(), "create") {
		t.Errorf("a refused template should not create anything, calls were %v", f.order())
	}
}

// Choices is the catalog, passed through with the titles the environment gives.
func TestChoicesIsTheCatalog(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()

	got, err := newDriver(t).Choices(context.Background(), envWithURL(srv.URL, "/sandbox"))
	if err != nil {
		t.Fatalf("Choices: %v", err)
	}
	if len(got) != 2 || got[0].ID != "all-in-one" || got[1].ID != "python" {
		t.Fatalf("Choices = %+v, want the catalog's two templates", got)
	}
}

// A configured template wins over the catalog's first, so a deployment can pin
// what a lab is made from without a caller choosing.
func TestProvisionPrefersTheConfiguredTemplate(t *testing.T) {
	f := &fakeSandboxlab{}
	srv := f.server("/sandbox")
	defer srv.Close()
	env := envWithURL(srv.URL, "/sandbox")
	env.Template = "python"

	got, err := newDriver(t).Provision(context.Background(), env, driver.ProvisionRequest{SessionID: "aaaaaaaa"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got.Template != "python" {
		t.Errorf("Template = %q, want the configured python", got.Template)
	}
	// A configured template needs no catalog lookup to resolve.
	if contains(f.order(), "catalog") {
		t.Errorf("a configured template should not need the catalog, calls were %v", f.order())
	}
}
