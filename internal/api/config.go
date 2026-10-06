package api

import (
	"net/http"
	"sort"

	"github.com/shaowenchen/labs/internal/buildinfo"
	"github.com/shaowenchen/labs/internal/session"
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
		// The TTL in seconds, so a reader can derive a running instance's
		// expiry from its creation time. An applab app has no expiry of its own
		// — the lab's clock is the session's, created + ttl — while a
		// sandboxlab sandbox reports its own.
		"session_ttl_seconds": int64(s.cfg.SessionTTL.Seconds()),
		"kinds":               list,
		// The repositories actually in play, and where the list came from. A
		// deployment serving one kind when it meant to serve two is a silent
		// mistake — nothing is broken, there is just less here than expected —
		// and this is what makes it visible: LABS_REPOS set to one repository
		// overrides the default, and the answer says so rather than leaving the
		// reader to notice a missing block on the page.
		"repos":      s.cfg.Repos,
		"repos_from": s.cfg.ReposSource,
		"configured": s.cfg.Usable(),
		"problems":   s.cfg.Problems,
		"limits": map[string]any{
			"max_sessions":        s.cfg.SessionCeiling(),
			"max_sessions_per_ip": s.cfg.MaxSessionsPerIP,
			"rate_limit_count":    s.cfg.RateLimitCount,
			"rate_limit_window":   s.cfg.RateLimitWindow.String(),
		},
		"environments": envStatusJSON(s.svc.Status(r.Context())),
		"labs":         liveJSON(s.svc.Live(r.Context())),
	})
}

// liveJSON renders what each environment is running, for the page's list. It is
// ids, states and times only — never a key: the key is the deliverable, shown
// once at creation, and a listing exists so it does not have to be shown again.
func liveJSON(lists []session.LiveLabs) []map[string]any {
	out := make([]map[string]any, 0, len(lists))
	for _, l := range lists {
		items := make([]map[string]any, 0, len(l.Items))
		for _, it := range l.Items {
			row := map[string]any{"id": it.ID, "state": it.State}
			if !it.CreatedAt.IsZero() {
				row["created_at"] = it.CreatedAt
			}
			if !it.ExpiresAt.IsZero() {
				row["expires_at"] = it.ExpiresAt
			}
			items = append(items, row)
		}
		out = append(out, map[string]any{
			"env_id": l.EnvID,
			"kind":   string(l.Kind),
			"labs":   items,
		})
	}
	return out
}
