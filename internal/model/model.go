// Package model holds the service's plain domain types.
//
// Nothing here imports another internal package, and nothing here knows about
// HTTP, GitHub or a driver. That is deliberate: Env, Session and Kind are the
// vocabulary every other package shares, so putting them somewhere leaf-like is
// what keeps the dependency graph a tree rather than a knot.
package model

import "time"

// Kind names a class of lab — which underlying project an environment runs.
type Kind string

const (
	// KindApplab is an AppLab deployment: push source, it builds and serves it.
	KindApplab Kind = "applab"

	// KindSandboxlab is a sandboxlab deployment: disposable shells, desktops
	// and browsers.
	KindSandboxlab Kind = "sandboxlab"
)

// Known reports whether k is a kind this service understands.
func (k Kind) Known() bool {
	return k == KindApplab || k == KindSandboxlab
}

// Env is one long-lived, pre-warmed lab environment, as configured.
//
// It is the static description of "where an environment lives and how to drive
// it" — not its live state. Whether it is up right now is what the driver's
// Ready answers; this is the address to ask.
type Env struct {
	// ID names the environment, and is what LABS_KEY_<ID> is derived from. It
	// is used in logs and in a session's env_id, so it must be stable.
	ID string `json:"id"`

	// Kind is which project this environment runs.
	Kind Kind `json:"kind"`

	// Repo is the GitHub repository whose workflow brings the environment up,
	// as "owner/repo". It must appear in LABS_REPOS.
	Repo string `json:"repo"`

	// Workflow is the workflow file name to dispatch, e.g. "debugger.yml".
	Workflow string `json:"workflow"`

	// Ref is the git ref to dispatch the workflow on.
	Ref string `json:"ref"`

	// Scheme is "https" unless a deployment says otherwise. Empty means https.
	Scheme string `json:"scheme,omitempty"`

	// Domain is the bare hostname the environment is served under, without a
	// scheme and without a path, e.g. "applab-1.example.com". It is expected to
	// be stable across runs, which is what makes polling for readiness possible
	// at all: a named tunnel keeps its hostname.
	Domain string `json:"domain"`

	// BasePath is the path the whole deployment is served under, with a leading
	// slash and no trailing one, e.g. "/applab". Empty means the root.
	BasePath string `json:"base_path"`

	// Template is the sandboxlab template a session creates. Ignored by applab.
	Template string `json:"template,omitempty"`

	// Slots are the app ids an applab environment lends out, one per concurrent
	// session. Empty for sandboxlab, which is counted by Capacity instead.
	Slots []string `json:"slots,omitempty"`

	// Capacity is how many concurrent sessions this environment may serve. For
	// applab it should equal len(Slots); it is authoritative for sandboxlab.
	Capacity int `json:"capacity"`

	// APIKey is the environment's own key, which labs holds in order to mint
	// per-session credentials. It is never serialised — a log line or a
	// /config response must not carry it. Empty means labs made one up and
	// hands it to the environment through the dispatch (see ManagedKey).
	APIKey string `json:"-"`

	// ManagedKey says labs chose this environment's key and passes it in the
	// dispatch, rather than the environment having been configured with a key
	// out of band. It is what decides whether the dispatch carries an api_key
	// input — one that is only sent when the workflow declares it, because
	// GitHub rejects a dispatch with an input the workflow does not know.
	ManagedKey bool `json:"-"`
}

// DispatchInputs is the workflow_dispatch input map for this environment.
//
// Both projects declare domain as a choice — only the hostnames their tunnel is
// configured for — so a domain is sent only when one is configured, and left
// out otherwise so the workflow uses its own default. The address is then read
// from the run log, which is how a deployment needs no domain at all.
//
// The api_key input is applab's alone: its debugger workflow accepts one and
// threads it into the environment (inputs.api_key || secrets.APPLAB_API_KEY), so
// labs can choose the key and hand it over at dispatch. sandboxlab's workflow
// takes no such input, so its key is configured out of band and never sent.
func (e Env) DispatchInputs(sessionHours string) map[string]string {
	out := map[string]string{
		"session_hours": sessionHours,
		"tunnel":        "cloudflare",
	}
	if e.Domain != "" {
		out["domain"] = e.Domain
	}
	if e.Kind == KindApplab && e.ManagedKey && e.APIKey != "" {
		out["api_key"] = e.APIKey
	}
	return out
}

// BaseURL is the environment's root address: scheme, host and base path.
//
// The API lives under BaseURL + "/api/v1/...", and for applab so does the
// console — one address serves everything, which is why a single field is
// enough and the two are not tracked separately.
func (e Env) BaseURL() string {
	scheme := e.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + e.Domain + e.BasePath
}

// Session is one delivered lab: what a caller was handed, and when it ends.
//
// The API key a caller receives is deliberately not a field. labs uses it once,
// at the moment it is minted, and never again — expiry is enforced by rotating
// the credential, not by re-reading it — so keeping it would be holding a
// secret for no purpose, and a compromise of the process's memory would be a
// compromise of every live credential.
type Session struct {
	// ID is the session's handle. It is minted from crypto/rand and is the
	// bearer of GET/DELETE /api/v1/labs/{id}, so it must be unguessable.
	ID string `json:"id"`

	// EnvID is the environment this session is using.
	EnvID string `json:"env_id"`

	// Kind is the environment's kind, copied here so a session can be read
	// without a second lookup.
	Kind Kind `json:"kind"`

	// ConsoleURL is what the caller opens — the environment's console.
	ConsoleURL string `json:"console_url"`

	// App is the applab slot this session occupies, if any.
	App string `json:"app,omitempty"`

	// SandboxID is the sandboxlab sandbox this session owns, if any.
	SandboxID string `json:"sandbox_id,omitempty"`

	// ClientIP is who was given this session, recorded for the per-address cap
	// and for an operator answering "where did these come from".
	ClientIP string `json:"client_ip"`

	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Expired reports whether the session's time is up, as of now.
func (s Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }
