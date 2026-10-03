package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/shaowenchen/labs/internal/buildinfo"
)

// getConfig answers the deployment's own shape — what it serves, how long a lab
// lasts, and how much of it there is — without a key. It is the first call a
// client makes, and it is what tells it whether to ask for an applab or a
// sandboxlab lab.
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	kinds := map[string]bool{}
	for _, env := range s.cfg.Envs {
		kinds[string(env.Kind)] = true
	}
	list := make([]string, 0, len(kinds))
	for k := range kinds {
		list = append(list, k)
	}
	sort.Strings(list)

	respond(w, http.StatusOK, map[string]any{
		"api_version": APIVersion,
		"version":     buildinfo.String(),
		"commit":      buildinfo.Commit,
		"build_time":  buildinfo.BuildTime,
		"session_ttl": s.cfg.SessionTTL.String(),
		"kinds":       list,
		"configured":  s.cfg.Usable(),
		"problems":    s.cfg.Problems,
		"limits": map[string]any{
			"max_sessions":        s.cfg.SessionCeiling(),
			"max_sessions_per_ip": s.cfg.MaxSessionsPerIP,
			"rate_limit_count":    s.cfg.RateLimitCount,
			"rate_limit_window":   s.cfg.RateLimitWindow.String(),
		},
		"environments": envStatusJSON(s.svc.Status(r.Context())),
	})
}

// ensure starts a cluster of each kind that is not up. The landing page calls
// it on load, so a cluster is coming before anyone asks for a lab — which is
// what lets the page show "starting" and only offer a lab once one is ready.
//
// It returns immediately with the configuration verdict, not the environment
// status: the status needs a readiness probe that can take seconds, and the
// caller is already fetching that from /api/v1/config. Its only job is to have
// started what was not up.
//
// It is safe to call repeatedly: an environment that is up is skipped, and a
// start does nothing when a run is already running or queued.
func (s *Server) ensure(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Usable() {
		s.svc.EnsureStarted(r.Context())
	}
	respond(w, http.StatusOK, map[string]any{
		"configured": s.cfg.Usable(),
		"problems":   s.cfg.Problems,
	})
}

// setKey records a key entered on the page for an environment whose configured
// key was refused. It is how a caller fixes a wrong key without a redeploy.
//
// The key is held in memory only — a restart forgets it — which is the right
// weight for a value the page asks for only when the configured one does not
// work. It is never logged and never returned.
func (s *Server) setKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if strings.TrimSpace(body.Key) == "" {
		fail(w, r, BadRequest("a key is required"))
		return
	}
	if !s.svc.SetKey(r.Context(), id, body.Key) {
		fail(w, r, NotFound("environment %q", id))
		return
	}
	respond(w, http.StatusOK, map[string]any{"id": id, "set": true})
}
