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
	"time"

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
	//
	// It is shown to callers too — /readyz carries it and the console prints it
	// under an environment that is not up — so it is a sentence about the state
	// rather than about the failure. The error behind it (a tunnel's 530, a
	// refused connection) belongs in the log, which is where a driver puts it;
	// NotAnswering is what those cases return instead.
	Message string
}

// NotAnswering is the verdict for an environment that did not answer at all.
//
// The several ways that happens — the tunnel in front of an environment with no
// run is a 530, a stopped one refuses the connection, a booting one times out —
// are the same thing to everyone but an operator: the environment is not up
// yet, which is the ordinary state between one run ending and the next starting.
// The error itself goes to the log and this goes on the page, so a visitor is
// told the state rather than handed an edge proxy's diagnostics to read.
const NotAnswering = "the environment is not answering yet"

// WrongDeployment is the verdict for an address that answered but is not the
// environment this service expects — the shape of the response was not one it
// knows.
//
// It is separate from NotAnswering because the reader's answer differs: this
// will not fix itself by waiting, so "not answering yet" would be a promise the
// service cannot keep. The page says so plainly, and the detail — which field
// was missing, which route answered — goes to the log, because that is a
// deployment mistake for whoever set it up rather than something a visitor can
// act on.
const WrongDeployment = "the environment is not answering as expected"

// ProvisionRequest is one session to mint, already authorized by the service's
// own limits and already recorded in the store.
type ProvisionRequest struct {
	// SessionID is the id the service minted for this session. A driver may use
	// it to name what it creates, so a sandbox or app traces back to a session.
	SessionID string

	// App is the applab slot reserved for this session. Empty for sandboxlab.
	App string

	// Template is the template the caller chose, for a kind that offers a choice
	// of them. Empty means "the environment's own default", which is what a
	// caller that named none gets.
	Template string
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

	// Template is the template actually used, when the kind offers a choice of
	// them. It is echoed back rather than assumed, because the driver is what
	// resolves a request that named none onto the environment's default.
	Template string
}

// Choice is one template a kind lets a caller pick from.
type Choice struct {
	// ID is what a request names, and what the driver is handed back.
	ID string

	// Title is the human-readable name, and Description the one line under it.
	// Either may be empty for an environment that names its templates as bare
	// ids, in which case the ID is all there is to show.
	Title       string
	Description string
}

// Live is one instance an environment is actually running — an applab app or a
// sandboxlab sandbox — as the environment reports it. It is read from the
// environment rather than from this service's records, so it is the truth even
// across a restart of labs, and it deliberately carries no credential.
type Live struct {
	// ID is the instance's identity: the app id, or the sandbox id.
	ID string

	// State is what the environment calls its state, for a list that wants to
	// show it ("running", "stopped").
	State string

	// CreatedAt is when the instance was made, and ExpiresAt when it ends. An
	// environment that has no expiry of its own leaves ExpiresAt zero, and the
	// reader falls back to the session's own clock.
	CreatedAt time.Time
	ExpiresAt time.Time
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

	// Choices lists the templates a caller may pick from, for a kind that offers
	// a choice of them. A kind that offers none returns nil and no error, which
	// is what a caller should read as "there is nothing to choose". A kind that
	// offers them but cannot be reached returns an error, so the page can say
	// the list is unavailable rather than that it is empty.
	Choices(ctx context.Context, env model.Env) ([]Choice, error)

	// Live lists the instances the environment is actually running. It is what
	// the page shows, which is why it is read from the environment rather than
	// from this service's own records: a service that restarted has forgotten
	// its sessions, and the instances are still there. It must carry no
	// credential.
	Live(ctx context.Context, env model.Env) ([]Live, error)

	// Reconcile ends anything the environment is still running that is not one
	// of the given live sessions — the pass that cleans up after this service
	// lost its state, or crashed between minting and recording. It must never
	// touch something it did not create.
	Reconcile(ctx context.Context, env model.Env, live []model.Session) error
}
