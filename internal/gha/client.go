// Package gha talks to the GitHub Actions REST API: it dispatches the workflows
// that bring environments up and reads back what those runs are doing.
//
// It is deliberately a thin client — five verbs, no retries of its own, no
// caching. The keep-warm loop below is the only caller and it already ticks on
// an interval, which is a better retry than a loop inside a request would be.
package gha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is a GitHub REST client scoped to the endpoints this service uses.
type Client struct {
	base  string
	token string
	hc    *http.Client
	now   func() time.Time

	mu       sync.Mutex
	branches map[string]string // repo -> its default branch, once looked up
}

// New returns a client. base is the REST root, "https://api.github.com" unless
// a test or GitHub Enterprise says otherwise.
func New(base, token string) *Client {
	return &Client{
		base:     strings.TrimRight(base, "/"),
		token:    token,
		hc:       &http.Client{Timeout: 30 * time.Second},
		now:      time.Now,
		branches: map[string]string{},
	}
}

// resolveRef turns an empty ref into the repository's default branch.
//
// An empty ref is the common case: the workflows this service dispatches live
// on whatever branch the repository publishes from, which is master for one
// project and main for the other, and hardcoding either dispatches a ref that
// does not exist — a workflow_dispatch against a missing branch is a 404 and no
// run at all, which looks exactly like nothing happening.
//
// The answer is cached per repository: it changes rarely and this is called on
// every listing and every dispatch.
func (c *Client) resolveRef(ctx context.Context, repo, ref string) (string, error) {
	if ref != "" {
		return ref, nil
	}

	c.mu.Lock()
	cached, ok := c.branches[repo]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo, nil, &out); err != nil {
		return "", fmt.Errorf("could not read %s's default branch: %w", repo, err)
	}
	if out.DefaultBranch == "" {
		return "", fmt.Errorf("%s reported no default branch", repo)
	}

	c.mu.Lock()
	c.branches[repo] = out.DefaultBranch
	c.mu.Unlock()
	return out.DefaultBranch, nil
}

// Run is one workflow run, reduced to what the keeper decides on.
type Run struct {
	ID         int64     `json:"id"`
	Status     string    `json:"status"`     // queued | in_progress | completed
	Conclusion string    `json:"conclusion"` // success | failure | cancelled | ... (completed only)
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"run_started_at"`
	HTMLURL    string    `json:"html_url"`
}

// begun is when the run started doing work: its start time if it has one, else
// when it was created.
func (r Run) begun() time.Time {
	if !r.StartedAt.IsZero() {
		return r.StartedAt
	}
	return r.CreatedAt
}

// Running reports whether the run has not finished.
func (r Run) Running() bool { return r.Status != "completed" }

// Dispatch asks GitHub to start a workflow run.
//
// A dispatch is asynchronous: a 204 means GitHub accepted the request, not that
// a run exists yet. Finding the run it created is DispatchAndWait's job.
func (c *Client) Dispatch(ctx context.Context, repo, workflow, ref string, inputs map[string]string) error {
	ref, err := c.resolveRef(ctx, repo, ref)
	if err != nil {
		return err
	}
	body := map[string]any{"ref": ref}
	if len(inputs) > 0 {
		body["inputs"] = inputs
	}
	path := fmt.Sprintf("/repos/%s/actions/workflows/%s/dispatches", repo, url.PathEscape(workflow))
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Runs lists recent runs of a workflow, newest first, filtered to manual
// dispatches so an unrelated push-triggered run is never mistaken for this
// service's environment.
func (c *Client) Runs(ctx context.Context, repo, workflow, ref string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 10
	}
	ref, err := c.resolveRef(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/actions/workflows/%s/runs?event=workflow_dispatch&branch=%s&per_page=%d",
		repo, url.PathEscape(workflow), url.QueryEscape(ref), limit)

	var out struct {
		WorkflowRuns []Run `json:"workflow_runs"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.WorkflowRuns, nil
}

// RunLogs returns a run's log as plain text.
//
// GitHub answers the log endpoint with a redirect to a signed URL on a blob
// host, so following redirects is what makes this work — the default client
// does, and the Authorization header is dropped on the cross-host hop by Go's
// own redirect handling, which is what the signed URL expects.
func (c *Client) RunLogs(ctx context.Context, repo string, runID int64) (string, error) {
	path := fmt.Sprintf("/repos/%s/actions/runs/%d/logs", repo, runID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &APIError{Status: resp.StatusCode, Method: http.MethodGet, Path: path, Body: strings.TrimSpace(string(raw))}
	}
	return string(raw), nil
}

// DispatchAndFind dispatches a run and returns it once GitHub has created it.
//
// The wait is bounded and short: a dispatched run appears within a few seconds
// when it appears at all, and the caller ticks again in a minute if it does not.
// Returning "not found" rather than blocking is what keeps a stuck GitHub from
// holding the keep-warm loop hostage.
func (c *Client) DispatchAndFind(ctx context.Context, repo, workflow, ref string, inputs map[string]string, within time.Duration) (Run, error) {
	dispatchedAt := c.now()
	if err := c.Dispatch(ctx, repo, workflow, ref, inputs); err != nil {
		return Run{}, err
	}

	deadline := dispatchedAt.Add(within)
	for {
		runs, err := c.Runs(ctx, repo, workflow, ref, 10)
		if err != nil {
			return Run{}, err
		}
		for _, r := range runs {
			// A run created at or after the dispatch, allowing a second of slack
			// for clock skew between this host and GitHub.
			if !r.CreatedAt.Before(dispatchedAt.Add(-time.Second)) {
				return r, nil
			}
		}
		if c.now().After(deadline) {
			return Run{}, fmt.Errorf("dispatched %s/%s but no run appeared within %s", repo, workflow, within)
		}
		select {
		case <-ctx.Done():
			return Run{}, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// do performs one request, decoding a JSON body into out when out is non-nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Method: method, Path: path, Body: strings.TrimSpace(string(raw)), Remaining: resp.Header.Get("X-RateLimit-Remaining")}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// APIError is a non-2xx answer from GitHub.
type APIError struct {
	Status    int
	Method    string
	Path      string
	Body      string
	Remaining string
}

func (e *APIError) Error() string {
	// The rate-limit remaining count is included when GitHub sends one, because
	// a 403 that is really "no requests left" reads identically to one that is
	// "wrong token" until that number is in the message.
	msg := fmt.Sprintf("github %s %s: %d", e.Method, e.Path, e.Status)
	if e.Remaining != "" {
		msg += " (rate limit remaining: " + e.Remaining + ")"
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// RateLimited reports whether the error is GitHub refusing for lack of quota.
func (e *APIError) RateLimited() bool {
	return e.Status == http.StatusForbidden && e.Remaining == "0"
}

// RetryAfter parses a Retry-After header value, when the caller has one.
func RetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second, true
	}
	return 0, false
}
