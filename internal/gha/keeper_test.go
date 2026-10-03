package gha

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
)

// fakeGitHub is a scripted Actions API: it records dispatches and answers run
// listings from whatever the test sets.
type fakeGitHub struct {
	mu         sync.Mutex
	runs       []Run
	dispatches []map[string]any
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
		// A real dispatch creates a run; mimic that, so a caller that waits for
		// one to appear (DispatchAndFind) sees it.
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
	return httptest.NewServer(mux)
}

func testKeeper(t *testing.T, f *fakeGitHub, now time.Time) *Keeper {
	t.Helper()
	srv := f.server()
	t.Cleanup(srv.Close)

	k := NewKeeper(KeeperConfig{
		Client:   New(srv.URL, "token"),
		Targets:  []Target{{ID: "applab-1", Repo: "o/r", Workflow: "debugger.yml", Ref: "main", Inputs: map[string]string{"domain": "a.example.com"}}},
		Interval: time.Minute,
		Lifetime: 4 * time.Hour,
		Margin:   45 * time.Minute,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	k.now = func() time.Time { return now }
	k.findTimeout = 2 * time.Second
	return k
}

// The load-bearing test: with a run already queued, a tick must NOT dispatch,
// because GitHub's default concurrency queue holds one pending run and a second
// dispatch cancels the first. A keeper that got this wrong would cancel its own
// successor every tick and the environment would never come back.
func TestTickDoesNotDispatchOverAQueuedRun(t *testing.T) {
	f := &fakeGitHub{}
	f.setRuns(Run{ID: 1, Status: "queued", CreatedAt: time.Now()})
	k := testKeeper(t, f, time.Now())

	k.Tick(context.Background())

	if n := f.dispatchCount(); n != 0 {
		t.Fatalf("dispatched %d times with a run already queued; want 0", n)
	}
}

func TestTickDispatchesWhenNothingIsRunning(t *testing.T) {
	f := &fakeGitHub{}
	f.setRuns() // no runs at all
	k := testKeeper(t, f, time.Now())

	k.Tick(context.Background())

	if n := f.dispatchCount(); n != 1 {
		t.Fatalf("dispatched %d times with nothing running; want 1", n)
	}
	got := f.lastDispatch()
	if got["ref"] != "main" {
		t.Errorf("dispatched ref = %v, want main", got["ref"])
	}
	inputs, _ := got["inputs"].(map[string]any)
	if inputs["domain"] != "a.example.com" {
		t.Errorf("dispatched domain = %v, want a.example.com", inputs["domain"])
	}
}

func TestTickDispatchesAfterARunCompletes(t *testing.T) {
	f := &fakeGitHub{}
	f.setRuns(Run{ID: 1, Status: "completed", Conclusion: "success", CreatedAt: time.Now().Add(-5 * time.Hour)})
	k := testKeeper(t, f, time.Now())

	k.Tick(context.Background())

	if n := f.dispatchCount(); n != 1 {
		t.Fatalf("dispatched %d times after a completed run; want 1", n)
	}
}

func TestTickLeavesAFreshRunAlone(t *testing.T) {
	now := time.Now()
	f := &fakeGitHub{}
	f.setRuns(Run{ID: 1, Status: "in_progress", StartedAt: now.Add(-10 * time.Minute)})
	k := testKeeper(t, f, now)

	k.Tick(context.Background())

	if n := f.dispatchCount(); n != 0 {
		t.Fatalf("dispatched %d times while a run is in its first hour; want 0", n)
	}
}

func TestTickQueuesASuccessorNearTheEndOfARun(t *testing.T) {
	now := time.Now()
	f := &fakeGitHub{}
	// Started 3h40m ago against a 4h lifetime and a 45m margin: past the point a
	// successor should be queued.
	f.setRuns(Run{ID: 1, Status: "in_progress", StartedAt: now.Add(-3*time.Hour - 40*time.Minute)})
	k := testKeeper(t, f, now)

	k.Tick(context.Background())

	if n := f.dispatchCount(); n != 1 {
		t.Fatalf("dispatched %d times near the end of a run; want 1", n)
	}
}

func TestTickDoesNotDispatchWhenListingFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	k := NewKeeper(KeeperConfig{
		Client:   New(srv.URL, "token"),
		Targets:  []Target{{ID: "e", Repo: "o/r", Workflow: "w.yml", Ref: "main"}},
		Interval: time.Minute, Lifetime: 4 * time.Hour, Margin: 45 * time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// If the listing fails the keeper cannot know whether a run is up, and a
	// blind dispatch could cancel a live run's successor. It must do nothing.
	k.Tick(context.Background())
}

func TestDispatchInputsAreSentVerbatim(t *testing.T) {
	f := &fakeGitHub{}
	f.setRuns()
	k := testKeeper(t, f, time.Now())
	k.targets[0].Inputs = map[string]string{
		"session_hours": "4",
		"tunnel":        "cloudflare",
		"domain":        "applab-1.example.com",
	}

	k.Tick(context.Background())

	inputs, _ := f.lastDispatch()["inputs"].(map[string]any)
	for k, want := range map[string]string{"session_hours": "4", "tunnel": "cloudflare", "domain": "applab-1.example.com"} {
		if inputs[k] != want {
			t.Errorf("input %q = %v, want %q", k, inputs[k], want)
		}
	}
}

func TestLifetimeForRun(t *testing.T) {
	cases := map[string]time.Duration{"1": time.Hour, "2": 2 * time.Hour, "4": 4 * time.Hour, "unlimited": 6 * time.Hour, "": 4 * time.Hour}
	for in, want := range cases {
		if got := LifetimeForRun(in); got != want {
			t.Errorf("LifetimeForRun(%q) = %s, want %s", in, got, want)
		}
	}
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

// A request that finds nothing running starts a run — this is what brings an
// environment up on a host with no keeper.
func TestEnsureRunningDispatchesWhenNothingRuns(t *testing.T) {
	f := &fakeGitHub{}
	f.setRuns() // nothing
	k := testKeeper(t, f, time.Now())

	if !k.EnsureRunning(context.Background(), k.targets[0]) {
		t.Fatal("EnsureRunning = false with nothing running; want a dispatch")
	}
	if n := f.dispatchCount(); n != 1 {
		t.Fatalf("dispatched %d times, want 1", n)
	}
}

// A request must not cancel a queued successor: the same rule the keeper turns
// on. A run already queued means an environment is coming, so nothing is sent.
func TestEnsureRunningDoesNotDispatchOverAQueuedRun(t *testing.T) {
	now := time.Now()
	f := &fakeGitHub{}
	f.setRuns(Run{ID: 1, Status: "queued", CreatedAt: now})
	k := testKeeper(t, f, now)

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
	k := testKeeper(t, f, now)

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
