// Package driver is how the service drives a lab environment.
//
// Everything labs-specific — the HTTP API, the session clock, the reaper — is
// written against the Driver interface and never against a particular project.
// applab and sandboxlab differ in how a per-session credential is minted and
// revoked, and that difference is the whole of what the two implementations
// below this package hold. A third project, or a proxy in front of one, is
// another implementation and nothing else changes.
package driver

import (
	"context"

	"github.com/shaowenchen/labs/internal/model"
)

// Ready is what a probe of an environment found.
type Ready struct {
	// Ready is whether the environment answered as a working deployment.
	Ready bool

	// ConsoleURL is the address a caller should be sent to, derived from what
	// the environment reported rather than assumed. Empty when not ready.
	ConsoleURL string

	// Unauthorized says the environment is up but refused the key the service
	// holds — so it is reachable, and only the key is wrong. It is separate from
	// not-ready because the answer is different: a key to enter, not a wait.
	Unauthorized bool

	// Message explains a not-ready verdict, in words worth logging.
	Message string
}

// ProvisionRequest is one session to mint, already authorized by the service's
// own limits and already recorded in the store.
type ProvisionRequest struct {
	// SessionID is the id the service minted for this session. A driver may use
	// it to name what it creates, so a sandbox or app traces back to a session.
	SessionID string

	// App is the applab slot reserved for this session. Empty for sandboxlab.
	App string
}

// Provisioned is what a driver made: the address to deliver, and the credential
// to hand over alongside it.
type Provisioned struct {
	// ConsoleURL is the address the caller opens.
	ConsoleURL string

	// APIKey is the credential the caller pastes into that address. It is meant
	// to be handed to the caller and is never written to the store.
	APIKey string

	// App is the applab slot used, if any.
	App string

	// SandboxID is the sandboxlab sandbox created, if any.
	SandboxID string

	// Warning is a caveat worth passing to the caller — for example, that the
	// credential is shared across sessions. Empty when there is nothing to say.
	Warning string
}

// Driver drives one kind of environment.
//
// Every method takes the environment it acts on, rather than the driver holding
// one, because the service runs several environments of the same kind and the
// driver is stateless between calls.
type Driver interface {
	// Kind is the kind of environment this driver handles.
	Kind() model.Kind

	// Ready probes the environment with no credential and reports whether it is
	// serving. A not-ready environment is not an error: it is the ordinary state
	// between one run ending and the next starting, and the caller answers 503
	// rather than 500.
	Ready(ctx context.Context, env model.Env) (Ready, error)

	// Provision mints one session's credential and returns where to send the
	// caller. It is called only after the service has reserved a slot, so a
	// driver may assume capacity exists.
	Provision(ctx context.Context, env model.Env, req ProvisionRequest) (Provisioned, error)

	// Release ends a session: it revokes the credential and stops whatever the
	// session was using. It must be idempotent — the explicit delete and the
	// reaper both call it, and either may run first.
	Release(ctx context.Context, env model.Env, sess model.Session) error

	// EnsureSlot makes sure the given applab slot exists as a record, so that a
	// later Provision has something to rotate a key for. It is a no-op for a
	// kind without slots. It is called at warm-up rather than per request so
	// that the request path stays fast.
	EnsureSlot(ctx context.Context, env model.Env, app string) error

	// Reconcile ends anything the environment is still running that is not one
	// of the given live sessions — the pass that cleans up after this service
	// lost its state, or crashed between minting and recording. It must never
	// touch something it did not create.
	Reconcile(ctx context.Context, env model.Env, live []model.Session) error
}
