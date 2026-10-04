package gha

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is a scripted Actions API: it records dispatches and answers run
// listings from whatever the test sets.
type fakeGitHub struct {
	mu         sync.Mutex
	runs       []Run
	dispatches []map[string]any
	cancels    []int64
}

func (f *fakeGitHub) setRuns(runs ...Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = runs
}

func (f *fakeGitHub) dispatchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dispatches)
}

// order is the sequence of mutating calls, so a test can assert that a cancel
// happened before a dispatch.
func (f *fakeGitHub) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.cancels)+len(f.dispatches))
	for _, id := range f.cancels {
		out = append(out, "cancel:"+strconv.FormatInt(id, 10))
	}
	for range f.dispatches {
		out = append(out, "dispatch")
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func (f *fakeGitHub) lastDispatch() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dispatches) == 0 {
		return nil
	}
	return f.dispatches[len(f.dispatches)-1]
}

func (f *fakeGitHub) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		// The default branch the service resolves an empty ref to.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_branch":"master"}`))
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/workflows/{wf}/dispatches", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.dispatches = append(f.dispatches, body)
		// A real dispatch creates a run; mimic that, so the next listing shows it.
		f.runs = append(f.runs, Run{ID: int64(len(f.dispatches)), Status: "queued", CreatedAt: time.Now()})
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/workflows/{wf}/runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		runs := append([]Run(nil), f.runs...)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		f.mu.Lock()
		f.cancels = append(f.cancels, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	return httptest.NewServer(mux)
}

func testStarter(t *testing.T, f *fakeGitHub) *Starter {
	t.Helper()
	srv := f.server()
	t.Cleanup(srv.Close)

	return NewStarter(StarterConfig{
		Client:  New(srv.URL, "token"),
		Targets: []Target{{ID: "applab-1", Repo: "o/r", Workflow: "debugger.yml", Ref: "main", Inputs: map[string]string{"domain": "a.example.com"}}},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestAPIErrorNamesRateLimit(t *testing.T) {
	e := &APIError{Status: 403, Method: "POST", Path: "/x", Remaining: "0"}
	if !e.RateLimited() {
		t.Error("a 403 with no remaining quota should read as rate limited")
	}
	if !strings.Contains(e.Error(), "rate limit") {
		t.Errorf("message should mention the rate limit: %s", e.Error())
	}
}

// Nothing running starts a run: this is what brings an environment up when a
// request needs one.
func TestEnsureRunningDispatchesWhenNothingRuns(t *testing.T) {
	f := &fakeGitHub{}
	f.setRuns() // nothing
	k := testStarter(t, f)

	if !k.EnsureRunning(context.Background(), k.targets[0]) {
		t.Fatal("EnsureRunning = false with nothing running; want a dispatch")
	}
	if n := f.dispatchCount(); n != 1 {
		t.Fatalf("dispatched %d times, want 1", n)
	}
}

// A run already queued is the environment coming up, and is used as it is: a
// second dispatch would replace it rather than add to it, so nothing is sent.
func TestEnsureRunningDoesNotDispatchOverAQueuedRun(t *testing.T) {
	now := time.Now()
	f := &fakeGitHub{}
	f.setRuns(Run{ID: 1, Status: "queued", CreatedAt: now})
	k := testStarter(t, f)

	if !k.EnsureRunning(context.Background(), k.targets[0]) {
		t.Fatal("EnsureRunning = false with a run queued; want true")
	}
	if n := f.dispatchCount(); n != 0 {
		t.Fatalf("dispatched %d times over a queued run, want 0", n)
	}
}

// A run already in progress means the environment is coming up; nothing sent.
func TestEnsureRunningLeavesALiveRunAlone(t *testing.T) {
	now := time.Now()
	f := &fakeGitHub{}
	f.setRuns(Run{ID: 1, Status: "in_progress", StartedAt: now.Add(-time.Minute)})
	k := testStarter(t, f)

	if !k.EnsureRunning(context.Background(), k.targets[0]) {
		t.Fatal("EnsureRunning = false with a live run; want true")
	}
	if n := f.dispatchCount(); n != 0 {
		t.Fatalf("dispatched %d times with a live run, want 0", n)
	}
}

// An empty ref is resolved to the repository's default branch. This is the bug
// that made a dispatch against a master-defaulted repository do nothing: the
// service hardcoded "main", the branch did not exist, and workflow_dispatch
// answered 404 with no run.
func TestDispatchResolvesTheDefaultBranch(t *testing.T) {
	f := &fakeGitHub{}
	srv := f.server()
	defer srv.Close()

	c := New(srv.URL, "token")
	if err := c.Dispatch(context.Background(), "o/r", "debugger.yml", "", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("Dispatch with an empty ref: %v", err)
	}
	got := f.lastDispatch()
	if got["ref"] != "master" {
		t.Errorf("dispatched ref = %v, want the repository's default branch (master)", got["ref"])
	}
}

// An explicit ref is used as given, without asking GitHub.
func TestDispatchHonoursAnExplicitRef(t *testing.T) {
	f := &fakeGitHub{}
	srv := f.server()
	defer srv.Close()

	c := New(srv.URL, "token")
	if err := c.Dispatch(context.Background(), "o/r", "debugger.yml", "release", nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got := f.lastDispatch()["ref"]; got != "release" {
		t.Errorf("dispatched ref = %v, want release", got)
	}
}
