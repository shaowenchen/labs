package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/session"
	"github.com/shaowenchen/labs/internal/store"
)

// createLabRequest is the body of POST /api/v1/labs.
//
// The kind is optional: a request that names none gets the deployment's default,
// so a caller with one kind configured does not have to know its name.
//
// The template is optional in a different way. It only means anything for a kind
// that offers a choice of them, and leaving it out there is not an error — it
// asks for the environment's own default. Which is also what makes the two
// shapes of request work: {"kind":"applab"} and {"kind":"sandboxlab",
// "template":"e2b"}.
type createLabRequest struct {
	Kind     string `json:"kind"`
	Template string `json:"template,omitempty"`
}

// labResponse is a delivered lab. It is the only response that carries the API
// key — the key is the deliverable and is shown once, here, and never on a
// subsequent read.
type labResponse struct {
	SessionID  string            `json:"session_id"`
	Kind       string            `json:"kind"`
	ConsoleURL string            `json:"console_url"`
	APIKey     string            `json:"api_key,omitempty"`
	ExpiresAt  time.Time         `json:"expires_at"`
	App        string            `json:"app,omitempty"`
	SandboxID  string            `json:"sandbox_id,omitempty"`
	Template   string            `json:"template,omitempty"`
	Links      map[string]string `json:"links,omitempty"`
}

// createLab delivers a lab, which is the product: one anonymous call, one
// working environment, valid for the session TTL.
func (s *Server) createLab(w http.ResponseWriter, r *http.Request) {
	// A deployment that is not configured cannot serve a lab, and saying so
	// plainly beats the generic "no environment is available" that an empty
	// environment list would otherwise produce — the reader needs to know to go
	// and set variables, not to wait.
	if !s.cfg.Usable() {
		fail(w, r, Errorf(http.StatusServiceUnavailable,
			"this deployment is not configured yet").
			Retryable().
			WithProblems(s.cfg.Problems))
		return
	}

	var req createLabRequest
	// A body is optional; an empty one selects the default kind, which is the
	// common case of "just give me a lab".
	if err := decodeOptionalJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	kind, err := s.resolveKind(req.Kind)
	if err != nil {
		fail(w, r, err)
		return
	}

	result, err := s.svc.Provision(r.Context(), session.ProvisionRequest{
		Kind:     kind,
		Template: strings.TrimSpace(req.Template),
	}, s.clientIP(r))
	if err != nil {
		fail(w, r, s.provisionError(err))
		return
	}

	links := map[string]string{"console": result.ConsoleURL}
	respond(w, http.StatusCreated, labResponse{
		SessionID:  result.Session.ID,
		Kind:       string(result.Session.Kind),
		ConsoleURL: result.ConsoleURL,
		APIKey:     result.APIKey,
		ExpiresAt:  result.Session.ExpiresAt,
		App:        result.Session.App,
		SandboxID:  result.Session.SandboxID,
		Template:   result.Session.Template,
		Links:      links,
	})
}

// getLab re-reads a session. It never returns the key: a caller re-reading a
// session already has the key, and an id that leaked must not yield a
// credential.
func (s *Server) getLab(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.svc.Get(id)
	if !ok {
		fail(w, r, NotFound("lab %q", id))
		return
	}
	respond(w, http.StatusOK, labResponse{
		SessionID:  sess.ID,
		Kind:       string(sess.Kind),
		ConsoleURL: sess.ConsoleURL,
		ExpiresAt:  sess.ExpiresAt,
		App:        sess.App,
		SandboxID:  sess.SandboxID,
		Template:   sess.Template,
		Links:      map[string]string{"console": sess.ConsoleURL},
	})
}

// deleteLab ends a session early. It is idempotent: deleting one that is already
// gone is success, because the caller's intent is satisfied either way.
func (s *Server) deleteLab(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.svc.Release(r.Context(), id); err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "could not end lab %q", id).Wrap(err))
		return
	}
	respond(w, http.StatusOK, map[string]string{"deleted": id})
}

// resolveKind turns a requested kind into one this deployment runs, applying the
// default when none was named.
func (s *Server) resolveKind(requested string) (model.Kind, error) {
	if requested == "" {
		// The first configured environment is the default. A deployment with one
		// kind gets it whichever spelling a caller omits.
		if len(s.cfg.Envs) == 0 {
			return "", Errorf(http.StatusServiceUnavailable, "no environments are configured").Retryable()
		}
		return s.cfg.Envs[0].Kind, nil
	}
	kind := model.Kind(requested)
	if !kind.Known() {
		return "", BadRequest("unknown kind %q: this deployment serves applab and sandboxlab", requested)
	}
	for _, env := range s.cfg.Envs {
		if env.Kind == kind {
			return kind, nil
		}
	}
	return "", BadRequest("this deployment does not serve %q", requested)
}

// provisionError maps a provisioning failure onto the right status.
//
// The distinctions a caller acts on: a full service is "come back" (503), an
// address that already holds its share is "not you right now" (429), no
// environment being up is "we are starting" (503), and an unknown kind or a
// template the deployment does not offer is the caller's to fix (400). All of
// these are retryable except the last two.
func (s *Server) provisionError(err error) *apiError {
	switch {
	case errors.Is(err, session.ErrNoSuchTemplate):
		return BadRequest("%s", err.Error())
	case errors.Is(err, session.ErrUnknownKind):
		return BadRequest("%s", err.Error())
	case errors.Is(err, session.ErrNoReadyEnv):
		// Reaching here means the deployment is configured, so the request path
		// has just tried to start an environment — either it did, and this is
		// "come back in a few minutes", or it could not reach GitHub to try.
		// Both are retryable; the environment takes minutes to boot, so a
		// caller cannot be served by this request either way.
		return Errorf(http.StatusServiceUnavailable,
			"no lab environment is available right now; one is being started, which takes a few minutes").
			Retryable().WithRetryAfter(120)
	case errors.Is(err, store.ErrAtCapacity):
		return Errorf(http.StatusServiceUnavailable, "all labs are in use right now; try again shortly").
			Retryable().WithRetryAfter(30)
	case errors.Is(err, store.ErrIPLimit):
		return Errorf(http.StatusTooManyRequests, "this address already holds its share of labs; wait for one to expire or end it").
			Retryable().WithRetryAfter(300)
	default:
		return Errorf(http.StatusInternalServerError, "could not create a lab").Wrap(err)
	}
}

// isEmptyBody reports whether a decode error is Go's EOF on an empty body, which
// is how a request with no body arrives.
func decodeOptionalJSON(r *http.Request, out any) *apiError {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return BadRequest("could not read the request body as JSON: %v", err)
	}
	return nil
}
