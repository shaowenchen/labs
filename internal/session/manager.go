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
	sawReady := false
	var limitErr error

	for _, env := range m.envsOf(kind) {
		drv := m.drivers[kind]
		if drv == nil {
			return Result{}, fmt.Errorf("%w: no driver for %q", ErrUnknownKind, kind)
		}

		r := m.envReady(ctx, drv, env)
		if !r.Ready {
			continue
		}
		sawReady = true

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
			return Result{}, fmt.Errorf("reserve a slot in %s: %w", env.ID, err)
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
			return Result{}, fmt.Errorf("provision in %s: %w", env.ID, err)
		}

		console := prov.ConsoleURL
		if console == "" {
			console = r.ConsoleURL
		}
		if err := m.store.Complete(sess.ID, console, prov.App, prov.SandboxID); err != nil {
			return Result{}, fmt.Errorf("record the session: %w", err)
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
		return Result{Session: sess, ConsoleURL: console, APIKey: prov.APIKey, Warning: prov.Warning}, nil
	}

	switch {
	case limitErr != nil:
		return Result{}, limitErr
	case sawReady:
		return Result{}, store.ErrAtCapacity
	default:
		return Result{}, fmt.Errorf("%w: no %s environment is up", ErrNoReadyEnv, kind)
	}
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

// Warm makes sure every environment has the records a later provision needs.
func (m *Manager) Warm(ctx context.Context) {
	for _, env := range m.cfg.Envs {
		drv := m.drivers[env.Kind]
		if drv == nil {
			continue
		}
		for _, app := range env.Slots {
			if err := drv.EnsureSlot(ctx, env, app); err != nil {
				m.log.Warn("could not ensure a slot", "env", env.ID, "app", app, "error", err)
			}
		}
	}
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
func (m *Manager) envReady(ctx context.Context, drv driver.Driver, env model.Env) driver.Ready {
	m.mu.Lock()
	if e, ok := m.ready[env.ID]; ok && m.now().Sub(e.at) < m.readyTTL {
		m.mu.Unlock()
		return e.r
	}
	m.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, m.readyTimeout)
	defer cancel()
	r, err := drv.Ready(probeCtx, env)
	if err != nil {
		// An error is the driver failing to ask, not the environment being down.
		// Treated as "not ready" so a bug here degrades to a retryable 503
		// rather than a request that hangs or a 500.
		m.log.Warn("readiness probe errored", "env", env.ID, "error", err)
		r = driver.Ready{Message: err.Error()}
	}

	m.mu.Lock()
	m.ready[env.ID] = readyEntry{at: m.now(), r: r}
	m.mu.Unlock()
	return r
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
