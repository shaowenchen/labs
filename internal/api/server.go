package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/shaowenchen/labs/internal/config"
	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/ratelimit"
	"github.com/shaowenchen/labs/internal/session"
)

// APIVersion tracks the route surface, not the build version: it changes when a
// client that understood the old shape would be wrong, and not otherwise.
const APIVersion = "v1"

// SessionService is the service's behaviour, as the HTTP layer uses it. It is an
// interface so the routes can be exercised without GitHub or a live environment.
type SessionService interface {
	Provision(ctx context.Context, kind model.Kind, clientIP string) (session.Result, error)
	Get(id string) (model.Session, bool)
	Release(ctx context.Context, id string) error
	Status(ctx context.Context) []session.EnvStatus
	ReadyAny(ctx context.Context) bool
}

// Deps is what the server needs.
type Deps struct {
	Config  config.Config
	Service SessionService
	Limiter *ratelimit.Limiter
	Log     *slog.Logger
}

// Server is the HTTP handler.
type Server struct {
	cfg     config.Config
	svc     SessionService
	limiter *ratelimit.Limiter
	log     *slog.Logger
	handler http.Handler
}

// New builds the server and its routes.
func New(d Deps) *Server {
	s := &Server{
		cfg:     d.Config,
		svc:     d.Service,
		limiter: d.Limiter,
		log:     d.Log,
	}
	s.handler = s.routes()
	return s
}

// ServeHTTP handles a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// routes registers everything.
//
// net/http's method-aware patterns are used rather than a hand-rolled dispatch,
// so a route's accepted methods are declared where it is registered and a wrong
// method is a 405 the mux produces rather than something a handler remembers to
// do.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated, and there is no authenticated route anywhere: the whole
	// point is that a caller needs no account. The only per-caller control is
	// the rate limit on the one route that creates something.
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/v1/config", s.getConfig)
	mux.HandleFunc("GET /api/v1/describe", s.describe)

	mux.HandleFunc("POST /api/v1/labs", s.rateLimited(s.createLab))
	mux.HandleFunc("GET /api/v1/labs/{id}", s.getLab)
	mux.HandleFunc("DELETE /api/v1/labs/{id}", s.deleteLab)

	// A known path reached with the wrong method is a 405 that says which
	// methods it takes, not the 404 a fall-through would give. The method-less
	// pattern is the less specific one, so a matching method still wins.
	mux.HandleFunc("/api/v1/labs", methodNotAllowed("POST"))
	mux.HandleFunc("/api/v1/labs/{id}", methodNotAllowed("GET, DELETE"))

	// The landing page, and the backdrop for everything else. It is registered
	// last and answers any path the routes above did not.
	mux.HandleFunc("/", apiNotFound(s.console))

	return mux
}

// methodNotAllowed answers a path reached with the wrong method.
func methodNotAllowed(allowed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allowed)
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: r.Method + " is not supported here"})
	}
}

// rateLimited bounds how often one address may create a lab and reports a
// Retry-After when it has had its share.
func (s *Server) rateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.limiter != nil {
			if ok, retryAfter := s.limiter.Allow(s.clientIP(r)); !ok {
				fail(w, r, Errorf(http.StatusTooManyRequests, "too many labs requested from this address; try again later").
					Retryable().
					WithRetryAfter(int(retryAfter.Seconds())+1))
				return
			}
		}
		next(w, r)
	}
}

// clientIP is the caller's address, from X-Forwarded-For only when the service
// is configured to sit behind a proxy that sets it.
//
// Trusting the header unconditionally would make every limit in this service
// bypassable by sending one, so it is off unless a deployment says otherwise —
// and a deployment should only say otherwise when its own port is not reachable
// from the internet.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustedProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The left-most entry is the original client; the rest are proxies
			// this deployment trusts.
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// apiNotFound answers unknown /api/ paths with a JSON 404 rather than the
// console's HTML, so a client generated from a stale spec reads a 404 rather
// than a 200 it will try to parse.
func apiNotFound(console http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "no such endpoint: " + r.URL.Path})
			return
		}
		console(w, r)
	}
}

// ── health and readiness ────────────────────────────────────────────────────

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ready answers 200 when at least one environment is up, and 503 otherwise. The
// body lists every environment so an operator can see which is missing without
// reading the logs.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	envs := s.svc.Status(r.Context())
	body := map[string]any{"environments": envStatusJSON(envs)}
	if s.svc.ReadyAny(r.Context()) {
		writeJSON(w, http.StatusOK, merge(body, map[string]any{"status": "ok"}))
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, merge(body, map[string]any{"status": "unavailable"}))
}

func envStatusJSON(envs []session.EnvStatus) []map[string]any {
	out := make([]map[string]any, 0, len(envs))
	for _, e := range envs {
		m := map[string]any{
			"id":       e.ID,
			"kind":     string(e.Kind),
			"ready":    e.Ready,
			"capacity": e.Capacity,
			"occupied": e.Occupied,
		}
		if e.ConsoleURL != "" {
			m["console_url"] = e.ConsoleURL
		}
		if e.Message != "" {
			m["message"] = e.Message
		}
		out = append(out, m)
	}
	return out
}

func merge(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}
