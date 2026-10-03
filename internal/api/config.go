package api

import (
	"net/http"
	"sort"

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
