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

	// running reports whether the environment has a run going, before its
	// address is probed at all. Nil means there is nothing to ask, and the probe
	// alone decides — which is right for a test, or a deployment with no token.
	running func(ctx context.Context, env model.Env) (bool, string, bool)

	mu    sync.Mutex
	ready map[string]readyEntry

	// choices is the last successful read of a kind's templates, kept so the
	// page's poll does not reach the environment once every few seconds. It is
	// keyed by kind and never expires: the list of a kind's templates is a
	// property of the environment's build, not of its moment, so a stale copy
	// is right far more often than a fresh read is worth the wait.
	choices map[model.Kind][]driver.Choice
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
		choices:      map[model.Kind][]driver.Choice{},
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

// WithRunnerCheck sets the function that reports whether an environment has a
// run going. known is false when the question cannot be answered.
func (m *Manager) WithRunnerCheck(fn func(ctx context.Context, env model.Env) (bool, string, bool)) *Manager {
	m.running = fn
	return m
}

// runningOn reports whether the environment has a run going, before anything is
// asked of its address. known is false when there is nothing to ask (a test, a
// deployment with no token) or when the question could not be answered (GitHub
// refused the listing) — in both cases the probe alone decides, because an
// environment may well be up when only the run listing is unavailable.
func (m *Manager) runningOn(ctx context.Context, env model.Env) (up bool, why string, known bool) {
	if m.running == nil {
		return true, "", false
	}
	return m.running(ctx, env)
}

// Provision delivers one lab.
//
// It tries every environment of the requested kind that is up and has room, and
// only after all of them declines does it decide which kind of "no" to return:
// an environment that is up but full is at capacity, and no environment up at
// all is not ready. The distinction matters to the caller — one is "we are
// busy", the other is "we are starting" — so it is made from the evidence
// rather than guessed.
// ProvisionRequest is what a caller asked for: which kind of lab, and — for a
// kind that offers a choice — which template to make it from.
type ProvisionRequest struct {
	Kind model.Kind

	// Template is the caller's choice. Empty means the environment's own
	// default, which is what a caller that named none gets.
	Template string
}

// Provision mints a lab of the requested kind and hands it back.
//
// The two failures a caller can act on are separated here: "nothing is up right
// now" and "everything that is up is full". Both leave the caller to come back,
// but only one of them is worth starting an environment for, so the difference
// is tracked through the pass rather than collapsed at the end.
func (m *Manager) Provision(ctx context.Context, req ProvisionRequest, clientIP string) (Result, error) {
	kind := req.Kind
	if !kind.Known() {
		return Result{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}

	lim := store.Limits{MaxTotal: m.cfg.SessionCeiling(), MaxPerIP: m.cfg.MaxSessionsPerIP}
	now := m.now()

	res, err, retry := m.provisionPass(ctx, req, clientIP, lim, now)
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

// ErrNoSuchTemplate is a template this kind does not offer. It is the caller's
// to fix — the alternative would be to hand them a lab they did not ask for —
// so it maps to 400 rather than to a retry.
var ErrNoSuchTemplate = errors.New("no such template")

// ChoicesFor reports the templates a kind offers, from the first environment of
// that kind that can be read. An environment that cannot be reached yields an
// empty list and no error: the page shows no template picker rather than an
// error, because a kind whose list is unavailable is still one a lab can be
// asked for — the request simply gets the environment's own default.
//
// The first successful read is kept and reused. Reading it means a request to
// the environment, and this is called from /config, which the page polls every
// few seconds; without the cache a poll that used to be local becomes a round
// trip per kind. A read that fails is not cached, so an environment coming up
// starts offering its templates without waiting for a restart.
func (m *Manager) ChoicesFor(ctx context.Context, kind model.Kind) []driver.Choice {
	m.mu.Lock()
	cached, ok := m.choices[kind]
	m.mu.Unlock()
	if ok {
		return cached
	}
	drv := m.drivers[kind]
	if drv == nil {
		return nil
	}
	for _, env := range m.envsOf(kind) {
		choices, err := drv.Choices(ctx, env)
		if err != nil {
			m.log.Warn("could not list an environment's templates", "env", env.ID, "error", err)
			continue
		}
		if len(choices) > 0 {
			m.mu.Lock()
			m.choices[kind] = choices
			m.mu.Unlock()
			return choices
		}
	}
	return nil
}

// checkTemplate refuses a template this kind does not offer, before any slot is
// reserved or credential minted.
//
// The check happens here rather than only in the driver because by the time the
// driver sees the request a slot has been claimed and a session recorded, and a
// bad template would have to be unwound. Asking first also lets the refusal name
// the templates that do exist.
func (m *Manager) checkTemplate(ctx context.Context, req ProvisionRequest) error {
	if req.Template == "" {
		return nil
	}
	choices := m.ChoicesFor(ctx, req.Kind)
	if len(choices) == 0 {
		// The list could not be read. The driver makes the final call with the
		// catalog in hand; guessing here would refuse a template that is fine.
		return nil
	}
	for _, c := range choices {
		if c.ID == req.Template {
			return nil
		}
	}
	ids := make([]string, 0, len(choices))
	for _, c := range choices {
		ids = append(ids, c.ID)
	}
	return fmt.Errorf("%w: %q for %s; this deployment offers %s",
		ErrNoSuchTemplate, req.Template, req.Kind, strings.Join(ids, ", "))
}

// provisionPass is one pass over the environments of a kind.
//
// It returns the delivered lab and done when one was made; an error when the
// attempt should stop; and retry when nothing was up but starting an
// environment might change that.
func (m *Manager) provisionPass(ctx context.Context, req ProvisionRequest, clientIP string, lim store.Limits, now time.Time) (Result, error, bool) {
	kind := req.Kind
	sawReady := false
	var limitErr error

	// A template this deployment does not offer is refused before anything is
	// claimed, so a typo costs a request rather than a slot.
	if err := m.checkTemplate(ctx, req); err != nil {
		return Result{}, err, false
	}

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
			Template:  req.Template,
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
		// The template the driver reports, not the one asked for: a caller that
		// named none is resolved onto a default by the driver, and what the
		// session records has to be what ran.
		if err := m.store.Complete(sess.ID, console, prov.App, prov.SandboxID, prov.Template); err != nil {
			return Result{}, fmt.Errorf("record the session: %w", err), false
		}
		sess.ConsoleURL = console
		sess.App = prov.App
		sess.SandboxID = prov.SandboxID
		sess.Template = prov.Template

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

// Status reports every environment's readiness and what it is running, in the
// order the answer depends on: whether a run is going, whether the service
// answers, and then what the service says it has.
//
// The run check gates the rest. An environment with no run has nothing serving
// its address, so there is no point asking it anything — the status is "no run
// is active", the count is zero, and the list is empty. Only when a run is going
// (or when that cannot be determined) is the service reached.
//
// Occupied is counted from the environment itself, not from this service's
// records: the instances are the truth, and a service that restarted has
// forgotten its sessions while the apps and sandboxes it made are still running.
// The service's own record is used only as the fallback when the environment
// cannot be read — better a number from a stale record than a zero that looks
// like an empty cluster.
func (m *Manager) Status(ctx context.Context) []EnvStatus {
	out := make([]EnvStatus, 0, len(m.cfg.Envs))
	for _, env := range m.cfg.Envs {
		st := EnvStatus{ID: env.ID, Kind: env.Kind, Capacity: env.Capacity}
		if up, why, known := m.runningOn(ctx, env); known && !up {
			// No run: report that, and stop. The service is not asked anything,
			// which is also what keeps a stopped environment from answering with
			// an edge proxy's tunnel error instead of the real reason.
			st.Message = why
			out = append(out, st)
			continue
		}
		st.Occupied = m.occupied(ctx, env)
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

// occupied is how many instances an environment is running, taken from the
// environment's own list — an applab slot it has an app for, a sandboxlab
// sandbox it has running. That list is what the page also shows beneath the
// number, so the count and the rows under it are the same source and cannot
// disagree.
//
// The service's own record is the fallback only when the environment cannot be
// read at all: then the list is empty for the wrong reason, and a stale number
// beats a zero that looks like an empty cluster.
func (m *Manager) occupied(ctx context.Context, env model.Env) int {
	drv := m.drivers[env.Kind]
	if drv == nil {
		return m.recorded(env)
	}
	live, err := drv.Live(ctx, env)
	if err != nil {
		m.log.Warn("could not list an environment's running instances", "env", env.ID, "error", err)
		return m.recorded(env)
	}
	return len(live)
}

// recorded is the fallback count, from this service's own store: applab's named
// slots, or sandboxlab's live sessions.
func (m *Manager) recorded(env model.Env) int {
	if env.Kind == model.KindSandboxlab {
		return len(m.liveFor(env.ID))
	}
	return len(m.store.OccupiedSlots(env.ID))
}

// LiveLabs is what an environment is running, for the page. It is the driver's
// list, so it reflects the environment rather than this service's memory, and
// it carries no credential.
type LiveLabs struct {
	EnvID string
	Kind  model.Kind
	Items []driver.Live
}

// Live reports every environment's running instances, in environment order.
// An environment that cannot be read yields an empty list rather than an error:
// the page should show the others, and the count already falls back to the
// service's record.
//
// The list is the same set the count comes from — see occupied — so the number
// and the rows beneath it cannot disagree.
func (m *Manager) Live(ctx context.Context) []LiveLabs {
	out := make([]LiveLabs, 0, len(m.cfg.Envs))
	for _, env := range m.cfg.Envs {
		entry := LiveLabs{EnvID: env.ID, Kind: env.Kind}
		if drv := m.drivers[env.Kind]; drv != nil {
			items, err := drv.Live(ctx, env)
			if err != nil {
				m.log.Warn("could not list an environment's running instances", "env", env.ID, "error", err)
			}
			entry.Items = items
		}
		out = append(out, entry)
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

// envReady decides whether an environment can serve a lab, in the order the
// answer actually depends on: first whether a run is going, then whether the
// service answers, and the service's own list is what the page shows.
//
// The run check comes first because everything after it is meaningless without
// it. An environment with no run has nothing serving its address at all, so
// probing it only yields a tunnel error — a 530 that says less than "no run is
// active" and sends the reader looking at the tunnel when the real answer is
// that no environment was started. A recent probe is reused so a burst of
// requests does not stampede a freshly started environment.
func (m *Manager) envReady(ctx context.Context, drv driver.Driver, env model.Env) driver.Ready {
	m.mu.Lock()
	if e, ok := m.ready[env.ID]; ok && m.now().Sub(e.at) < m.readyTTL {
		m.mu.Unlock()
		return e.r
	}
	m.mu.Unlock()

	// Step one: is there a run at all?
	if up, why, known := m.runningOn(ctx, env); known && !up {
		r := driver.Ready{Message: why}
		m.remember(env.ID, r)
		return r
	}

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
		//
		// The error goes to the log and NotAnswering goes back, which is what a
		// driver does with the same case: what a caller can act on is the state,
		// and an error string here is this service's own internals — a URL, a
		// decode failure — shown to a visitor who cannot act on any of it.
		m.log.Warn("readiness probe errored", "env", env.ID, "error", err)
		r = driver.Ready{Message: driver.NotAnswering}
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
