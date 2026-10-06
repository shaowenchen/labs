package api

import (
	"net/http"

	"github.com/shaowenchen/labs/internal/buildinfo"
)

// getDescribe is the contract a client reads before it has been given anything:
// what this deployment serves and how to call it. It needs no key, like every
// other route here.
func (s *Server) describe(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]any{
		"summary":     "Labs hands out a working applab or sandboxlab environment to anyone who asks, valid for the session TTL. POST /api/v1/labs returns the console address and the key to use there.",
		"api_version": APIVersion,
		"version":     buildinfo.String(),
		"auth":        "none — every route is anonymous",
		"endpoints": []map[string]any{
			{"method": "POST", "path": "/api/v1/labs", "doc": "create a lab; body {\"kind\":\"applab|sandboxlab\", \"template\":\"...\"} optional, and template applies to a kind that offers a choice — see templates in GET /api/v1/config"},
			{"method": "GET", "path": "/api/v1/labs/{id}", "doc": "read a lab back, without its key"},
			{"method": "DELETE", "path": "/api/v1/labs/{id}", "doc": "end a lab early"},
			{"method": "GET", "path": "/api/v1/config", "doc": "the deployment's shape, its environments, and the templates each kind offers"},
			{"method": "GET", "path": "/healthz", "doc": "liveness"},
			{"method": "GET", "path": "/readyz", "doc": "whether any environment is up"},
		},
		"how_to": map[string]any{
			"bash":   "curl -sX POST https://<labs>/api/v1/labs -d '{\"kind\":\"applab\"}'",
			"then":   "open the console_url it returns and paste the api_key",
			"expiry": s.cfg.SessionTTL.String(),
		},
	})
}
