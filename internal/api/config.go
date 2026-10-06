package api

import (
	"context"
	"net/http"
	"sort"

	"github.com/shaowenchen/labs/internal/buildinfo"
	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/session"
)

// getConfig answers the deployment's own shape — what it serves, how long a lab
// lasts, and how much of it there is — without a key. It is the first call a
// client makes, and it is what tells it whether to ask for an applab or a
// sandboxlab lab, and what it may choose within one.
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
		// The repositories actually in play, and per kind where each list came
		// from. A deployment serving one kind when it meant to serve two is a
		// silent mistake — nothing is broken, there is just less here than
		// expected — and this is what makes it visible: the answer says which
		// variable decided each kind rather than leaving the reader to notice a
		// missing block on the page.
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
		"templates":    s.templatesJSON(r.Context()),
	})
}

// templatesJSON is the templates each kind offers, keyed by kind. A kind that
// offers no choice is absent rather than present-and-empty, so a reader can tell
// "there is nothing to pick here" from "the picker failed to load" — which is
// the difference between hiding it and saying so.
func (s *Server) templatesJSON(ctx context.Context) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, kind := range s.kinds() {
		choices := s.svc.ChoicesFor(ctx, kind)
		if len(choices) == 0 {
			continue
		}
		rows := make([]map[string]any, 0, len(choices))
		for _, c := range choices {
			row := map[string]any{"id": c.ID}
			if c.Title != "" {
				row["title"] = c.Title
			}
			if c.Description != "" {
				row["description"] = c.Description
			}
			rows = append(rows, row)
		}
		out[string(kind)] = rows
	}
	return out
}

// kinds is the distinct kinds this deployment serves, in a stable order.
func (s *Server) kinds() []model.Kind {
	seen := map[model.Kind]bool{}
	out := make([]model.Kind, 0, len(s.cfg.Envs))
	for _, env := range s.cfg.Envs {
		if seen[env.Kind] {
			continue
		}
		seen[env.Kind] = true
		out = append(out, env.Kind)
	}
	return out
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
