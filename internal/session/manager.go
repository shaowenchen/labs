// Package session turns a request for a lab into a delivered one, and back
// again.
//
// It is the only place that knows the order of the three things provisioning
// takes — reserve a slot, mint a credential, record the address — and the whole
// reason that order is written down once is that each step can fail and the
// steps before it must be undone. Reserving before minting is what makes two
// requests arriving together take different slots; recording after minting is
// what makes a crash in between leave a slot that Reconcile can find and clear.
package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/shaowenchen/labs/internal/config"
	"github.com/shaowenchen/labs/internal/driver"
	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/store"
)

// Errors a caller classifies into a status.
var (
	// ErrUnknownKind is a request for a kind this deployment does not run.
	ErrUnknownKind = errors.New("session: unknown kind")

	// ErrNoReadyEnv is no environment of the requested kind being up. It is the
	// ordinary state during a reboot, answered as "try again" rather than an
	// error.
	ErrNoReadyEnv = errors.New("session: no environment is ready")

	// ErrNotFound is a session id that is not recorded.
	ErrNotFound = errors.New("session: no such session")
)

// Result is a delivered lab, as the API reports it.
type Result struct {
	Session    model.Session
	ConsoleURL string
	APIKey     string
	Warning    string
}

// Manager provisions and releases sessions across the environments.
type Manager struct {
	cfg     config.Config
	store   *store.Store
	drivers map[model.Kind]driver.Driver
	log     *slog.Logger

	// now is the clock, so the session tests do not have to sleep. It is a
	// field rather than calling time.Now directly, which is the one seam a
	// time-dependent component needs.
	now func() time.Time

	// readyTTL is how long a readiness probe is trusted before it is redone.
	readyTTL time.Duration

	// readyTimeout bounds one probe, so a readiness check that reaches an
	// environment which accepts a connection and then never answers cannot hold
	// /readyz — or a provision — open indefinitely.
	readyTimeout time.Duration

	// discover finds where an environment lives when it was not configured with
	// a domain: it reads the address the environment printed in its own run log.
	// Nil when no discovery is available, and it is consulted only for
	// environments whose domain is empty.
	discover func(ctx context.Context, env model.Env) (string, string)

	// start makes sure an environment is coming up, dispatching a run if none is
	// active. It runs when a lab is created and finds nothing up — that request
	// is what brings an environment up. Nil means nothing can start one, and a
	// request that finds none up is answered as such.
	start func(ctx context.Context, env model.Env) bool

	mu    sync.Mutex
	ready map[string]readyEntry
}

type readyEntry struct {
	at time.Time
	r  driver.Ready
}

// New builds a manager.
func New(cfg config.Config, st *store.Store, drivers map[model.Kind]driver.Driver, log *slog.Logger) *Manager {
	return &Manager{
		cfg:          cfg,
		store:        st,
		drivers:      drivers,
		log:          log,
		now:          time.Now,
		readyTTL:     15 * time.Second,
		readyTimeout: 6 * time.Second,
		ready:        map[string]readyEntry{},
	}
}

// WithClock replaces the clock, for tests.
func (m *Manager) WithClock(now func() time.Time) *Manager {
	m.now = now
	return m
}

// WithDiscovery sets the function used to find an environment's address when it
// was not configured with one. It returns the address, or an empty address and
// a message saying why it could not be found — which is what the page shows, so
// a discovery that fails on a real reason (a missing token, an unreadable log)
// does not look like a cluster that is merely slow.
func (m *Manager) WithDiscovery(fn func(ctx context.Context, env model.Env) (string, string)) *Manager {
	m.discover = fn
	return m
}

// WithStarter sets the function used to start an environment when a request
// finds none up.
func (m *Manager) WithStarter(fn func(ctx context.Context, env model.Env) bool) *Manager {
	m.start = fn
	return m
}

// Provision delivers one lab.
//
// It tries every environment of the requested kind that is up and has room, and
// only after all of them declines does it decide which kind of "no" to return:
// an environment that is up but full is at capacity, and no environment up at
// all is not ready. The distinction matters to the caller — one is "we are
// busy", the other is "we are starting" — so it is made from the evidence
// rather than guessed.
func (m *Manager) Provision(ctx context.Context, kind model.Kind, clientIP string) (Result, error) {
	if !kind.Known() {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}

	lim := store.Limits{MaxTotal: m.cfg.SessionCeiling(), MaxPerIP: m.cfg.MaxSessionsPerIP}
	now := m.now()

	res, err, retry := m.provisionPass(ctx, kind, clientIP, lim, now)
	if err != nil || !retry {
		return res, err
	}

	// Nothing is up. If something can start one, start it — a caller asking for
	// a lab is what brings an environment up. The environment takes minutes to
	// boot, so this request cannot wait for it: it answers "not yet", and the
	// environment it started is there for the next one.
	if m.startOne(ctx, kind) {
		return Result{}, fmt.Errorf("%w: no %s environment was up, so one is being started", ErrNoReadyEnv, kind)
	}
	return Result{}, fmt.Errorf("%w: no %s environment is up", ErrNoReadyEnv, kind)
}

// provisionPass is one pass over the environments of a kind.
//
// It returns the delivered lab and done when one was made; an error when the
// attempt should stop; and retry when nothing was up but starting an
// environment might change that.
func (m *Manager) provisionPass(ctx context.Context, kind model.Kind, clientIP string, lim store.Limits, now time.Time) (Result, error, bool) {
	sawReady := false
	var limitErr error

	for _, env := range m.envsOf(kind) {
		drv := m.drivers[kind]
		if drv == nil {
			return Result{}, fmt.Errorf("%w: no driver for %q", ErrUnknownKind, kind), false
		}

		r := m.envReady(ctx, drv, env)
		if !r.Ready {
			continue
		}
		sawReady = true

		// An environment whose address was discovered rather than configured is
		// probed and provisioned at that address: the driver needs the same host
		// the probe succeeded against.
		if base, _ := m.baseURLFor(ctx, env); base != "" {
			if resolved, ok := envAt(env, base); ok {
				env = resolved
			}
		}

		sess := model.Session{
			ID:        NewID(),
			EnvID:     env.ID,
			Kind:      kind,
			ClientIP:  clientIP,
			CreatedAt: now,
			ExpiresAt: now.Add(m.cfg.SessionTTL),
		}

		app, err := m.store.Reserve(env, sess, lim)
		switch {
		case errors.Is(err, store.ErrNoSlot):
			// This environment is full; another may not be.
			continue
		case errors.Is(err, store.ErrAtCapacity), errors.Is(err, store.ErrIPLimit):
			// Keep the most specific reason: a caller who has used their own
			// share wants to hear that, not that the service is busy.
			if limitErr == nil || errors.Is(err, store.ErrIPLimit) {
				limitErr = err
			}
			continue
		case err != nil:
			return Result{}, fmt.Errorf("reserve a slot in %s: %w", env.ID, err), false
		}

		sess.App = app
		prov, err := drv.Provision(ctx, env, driver.ProvisionRequest{
			SessionID: sess.ID,
			App:       app,
		})
		if err != nil {
			// Give the slot back. The session never reached the caller, so
			// nothing else will ever release it.
			if dropErr := m.store.Drop(sess.ID); dropErr != nil {
				m.log.Warn("could not release a slot after a failed provision", "env", env.ID, "session", sess.ID, "error", dropErr)
			}
			return Result{}, fmt.Errorf("provision in %s: %w", env.ID, err), false
		}

		console := prov.ConsoleURL
		if console == "" {
			console = r.ConsoleURL
		}
		if err := m.store.Complete(sess.ID, console, prov.App, prov.SandboxID); err != nil {
			return Result{}, fmt.Errorf("record the session: %w", err), false
		}
		sess.ConsoleURL = console
		sess.App = prov.App
		sess.SandboxID = prov.SandboxID

		m.log.Info("delivered a lab",
			"session", sess.ID,
			"kind", kind,
			"env", env.ID,
			"app", prov.App,
			"sandbox", prov.SandboxID,
			"expires_at", sess.ExpiresAt.Format(time.RFC3339),
		)
		return Result{Session: sess, ConsoleURL: console, APIKey: prov.APIKey, Warning: prov.Warning}, nil, false
	}

	// Nothing was delivered. If a limit stopped every environment, that is the
	// answer and starting one would not help; otherwise it is worth trying to
	// start one.
	switch {
	case limitErr != nil:
		return Result{}, limitErr, false
	case sawReady:
		return Result{}, store.ErrAtCapacity, false
	default:
		return Result{}, nil, true
	}
}

// startOne dispatches a run for one environment of the kind, so a request that
// found nothing up gets an environment started. It reports whether a start was
// attempted, which is what decides whether a second pass is worth making.
func (m *Manager) startOne(ctx context.Context, kind model.Kind) bool {
	if m.start == nil {
		return false
	}
	for _, env := range m.envsOf(kind) {
		if m.start(ctx, env) {
			return true
		}
	}
	return false
}

// Get returns a recorded session.
func (m *Manager) Get(id string) (model.Session, bool) {
	return m.store.Get(id)
}

// Release ends a session and frees what it held. It is idempotent.
func (m *Manager) Release(ctx context.Context, id string) error {
	sess, ok := m.store.Get(id)
	if !ok {
		return nil
	}
	env, ok := m.cfg.EnvByID(sess.EnvID)
	if !ok {
		// The environment is no longer configured — the deployment changed out
		// from under a live session. The credential cannot be revoked through a
		// driver that no longer exists, but the session is dropped so it stops
		// counting against the limits.
		m.log.Warn("releasing a session whose environment is no longer configured", "session", id, "env", sess.EnvID)
		return m.store.Drop(id)
	}
	if drv := m.drivers[sess.Kind]; drv != nil {
		if err := drv.Release(ctx, env, sess); err != nil {
			return err
		}
	}
	return m.store.Drop(id)
}

// Expire releases every session whose time is up and reports how many it ended.
func (m *Manager) Expire(ctx context.Context) (int, error) {
	expired := m.store.Expired(m.now())
	n := 0
	var firstErr error
	for _, sess := range expired {
		if err := m.Release(ctx, sess.ID); err != nil {
			m.log.Warn("could not release an expired session", "session", sess.ID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// Reconcile runs the per-environment cleanup pass.
func (m *Manager) Reconcile(ctx context.Context) error {
	var firstErr error
	for _, env := range m.cfg.Envs {
		drv := m.drivers[env.Kind]
		if drv == nil {
			continue
		}
		live := m.liveFor(env.ID)
		if err := drv.Reconcile(ctx, env, live); err != nil {
			m.log.Warn("reconcile failed", "env", env.ID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// EnvStatus is one environment's readiness, for /readyz and /config.
type EnvStatus struct {
	ID         string
	Kind       model.Kind
	Ready      bool
	ConsoleURL string
	Message    string
	Occupied   int
	Capacity   int

	// Unauthorized says the environment is up but refused the key this service
	// holds. The page offers a way to enter the right one.
	Unauthorized bool
}

// Status reports every environment's readiness.
func (m *Manager) Status(ctx context.Context) []EnvStatus {
	out := make([]EnvStatus, 0, len(m.cfg.Envs))
	for _, env := range m.cfg.Envs {
		st := EnvStatus{ID: env.ID, Kind: env.Kind, Capacity: env.Capacity, Occupied: len(m.store.OccupiedSlots(env.ID))}
		if env.Kind == model.KindSandboxlab {
			// sandboxlab has no named slots; report live sessions instead.
			st.Occupied = 0
		}
		if drv := m.drivers[env.Kind]; drv != nil {
			r := m.envReady(ctx, drv, env)
			st.Ready = r.Ready
			st.ConsoleURL = r.ConsoleURL
			st.Message = r.Message
			st.Unauthorized = r.Unauthorized
		}
		out = append(out, st)
	}
	return out
}

// ReadyAny reports whether at least one environment is up.
func (m *Manager) ReadyAny(ctx context.Context) bool {
	for _, env := range m.cfg.Envs {
		if drv := m.drivers[env.Kind]; drv != nil && m.envReady(ctx, drv, env).Ready {
			return true
		}
	}
	return false
}

// envReady probes an environment, reusing a recent probe so a burst of requests
// does not stampede a freshly started environment.
//
// An environment with no configured domain is probed at the address its own run
// log reported, discovered once and cached; until that address is known it is
// simply not ready, which is the ordinary state while its run is still coming up.
func (m *Manager) envReady(ctx context.Context, drv driver.Driver, env model.Env) driver.Ready {
	m.mu.Lock()
	if e, ok := m.ready[env.ID]; ok && m.now().Sub(e.at) < m.readyTTL {
		m.mu.Unlock()
		return e.r
	}
	m.mu.Unlock()

	probe := env
	if probe.Domain == "" {
		base, why := m.baseURLFor(ctx, env)
		if base == "" {
			r := driver.Ready{Message: why}
			m.remember(env.ID, r)
			return r
		}
		probe.Domain = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
		if strings.HasPrefix(base, "http://") {
			probe.Scheme = "http"
		}
	}

	// probe already carries the configured key
	probeCtx, cancel := context.WithTimeout(ctx, m.readyTimeout)
	defer cancel()
	r, err := drv.Ready(probeCtx, probe)
	if err != nil {
		// An error is the driver failing to ask, not the environment being down.
		// Treated as "not ready" so a bug here degrades to a retryable 503
		// rather than a request that hangs or a 500.
		m.log.Warn("readiness probe errored", "env", env.ID, "error", err)
		r = driver.Ready{Message: err.Error()}
	}

	m.remember(env.ID, r)
	return r
}

// envAt returns env with its scheme and host replaced by those of base, keeping
// its base path. It is how a discovered address is folded into the environment
// the driver is handed, so the driver reaches exactly what the probe reached.
func envAt(env model.Env, base string) (model.Env, bool) {
	scheme, rest := "https", base
	if i := strings.Index(base, "://"); i >= 0 {
		scheme, rest = base[:i], base[i+3:]
	}
	host := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host = rest[:i]
	}
	if host == "" {
		return env, false
	}
	env.Scheme = scheme
	env.Domain = host
	return env, true
}

// baseURLFor returns an environment's address, discovering it from the run log
// when it was not configured. A discovered address is cached for the life of the
// process; a configured one never needs this.
//
// The second return is a message for the page when there is no address yet —
// why discovery could not find one, so a failure with a real cause does not look
// like a cluster that is merely slow.
func (m *Manager) baseURLFor(ctx context.Context, env model.Env) (string, string) {
	if env.Domain != "" {
		return env.BaseURL(), ""
	}
	if m.discover == nil {
		return "", "the cluster is starting; its address has not been announced yet"
	}
	return m.discover(ctx, env)
}

func (m *Manager) remember(envID string, r driver.Ready) {
	m.mu.Lock()
	m.ready[envID] = readyEntry{at: m.now(), r: r}
	m.mu.Unlock()
}

func (m *Manager) envsOf(kind model.Kind) []model.Env {
	var out []model.Env
	for _, env := range m.cfg.Envs {
		if env.Kind == kind {
			out = append(out, env)
		}
	}
	return out
}

func (m *Manager) liveFor(envID string) []model.Session {
	var out []model.Session
	for _, s := range m.store.Sessions() {
		if s.EnvID == envID {
			out = append(out, s)
		}
	}
	return out
}

// NewID mints a session id: 16 bytes of crypto/rand as 32 hex characters. It is
// the same length as the credentials the sibling projects mint, and long enough
// that guessing is not part of anyone's threat model.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not a condition to paper over: an id that is
		// predictable is a session anyone can read and end. Panicking is the
		// right failure, and it does not happen on a working system.
		panic("session: reading crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// EqualID compares two ids in constant time, so that finding one by guessing is
// not made easier by how long the comparison took.
func EqualID(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
