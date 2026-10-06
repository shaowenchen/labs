// Package store records the sessions this service has handed out.
//
// It keeps them in memory, guarded by a mutex. There is deliberately no file:
// the service is a single long-lived process, and a lab is a disposable thing
// that lasts two hours, so the state it needs to hold is small and short-lived
// enough that a volume to persist it would cost more to operate than it saves.
//
// Two consequences follow, and both are acceptable for a lab service rather
// than accidents:
//
//   - A restart forgets the live sessions. On the next reconciliation every
//     slot looks unheld, so every outstanding credential is rotated away — a
//     restart ends the labs that were in flight, and leaves none alive and
//     unaccounted for. That is the safer of the two failure directions: it can
//     cut a session short, but it cannot leave a credential working.
//   - The state is per-process, so the service runs one replica. Two would
//     each hold their own half of the sessions and hand the same slot out
//     twice. Persisting to a shared store is what would change that, and this
//     is where it would go.
//
// The store never holds a credential: a session's API key is used once, when it
// is minted, and expiry is enforced by rotating the credential rather than by
// re-reading it, so the key has no reason to be kept.
package store

import (
	"errors"
	"sync"
	"time"

	"github.com/shaowenchen/labs/internal/model"
)

// Sentinel errors a caller maps onto a status or a decision.
var (
	// ErrNotFound is a session that is not recorded.
	ErrNotFound = errors.New("store: no such session")

	// ErrExists is a session id already in use, which is a collision in a
	// random id and therefore a bug worth surfacing rather than overwriting.
	ErrExists = errors.New("store: session already exists")

	// ErrNoSlot is an applab environment with no free app slot.
	ErrNoSlot = errors.New("store: no free slot in the environment")

	// ErrAtCapacity is the global concurrent-session cap being reached.
	ErrAtCapacity = errors.New("store: at capacity")

	// ErrIPLimit is a client address having as many concurrent sessions as it
	// is allowed.
	ErrIPLimit = errors.New("store: this address already has its share of sessions")
)

// Limits is what Reserve enforces before it claims anything. A zero field means
// no limit.
type Limits struct {
	MaxTotal int
	MaxPerIP int
}

// Store is the session record, held in memory.
type Store struct {
	mu       sync.Mutex
	sessions []model.Session

	// slots maps an environment id to its app ids and the session occupying
	// each, so slot occupancy and the session list cannot disagree: they are
	// updated under one lock.
	slots map[string]map[string]string
}

// New returns an empty store.
func New() *Store {
	return &Store{slots: map[string]map[string]string{}}
}

// Sessions returns every recorded session, newest first.
func (s *Store) Sessions() []model.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot()
}

// Get returns the session with the given id.
func (s *Store) Get(id string) (model.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ID == id {
			return sess, true
		}
	}
	return model.Session{}, false
}

// Expired returns every session whose time is up, as of now.
func (s *Store) Expired(now time.Time) []model.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Session
	for _, sess := range s.sessions {
		if sess.Expired(now) {
			out = append(out, sess)
		}
	}
	return out
}

// CountForIP is how many sessions the address currently holds.
func (s *Store) CountForIP(ip string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sess := range s.sessions {
		if sess.ClientIP == ip {
			n++
		}
	}
	return n
}

// Total is how many sessions are recorded.
func (s *Store) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// OccupiedSlots returns the app ids an applab environment currently has in use,
// mapped to the session holding each. It is what Reconcile compares against.
func (s *Store) OccupiedSlots(envID string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for app, sessID := range s.slots[envID] {
		out[app] = sessID
	}
	return out
}

// Reserve claims a session slot and records the session, all under one lock, so
// that the limit it checks and the slot it claims cannot be raced apart by two
// requests arriving together.
//
// For an applab environment the slot is a named app id from env.Slots; the
// claimed one is returned and recorded against the session. sandboxlab
// environments are not slotted, only counted, and Reserve returns "".
//
// The session is recorded with whatever fields the caller has already filled in
// — id, env, address, times — and ConsoleURL empty, because the address is not
// known until the driver has minted the credential. Complete fills it in.
func (s *Store) Reserve(env model.Env, sess model.Session, lim Limits) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.sessions {
		if existing.ID == sess.ID {
			return "", ErrExists
		}
	}
	if lim.MaxTotal > 0 && len(s.sessions) >= lim.MaxTotal {
		return "", ErrAtCapacity
	}
	if lim.MaxPerIP > 0 {
		n := 0
		for _, existing := range s.sessions {
			if existing.ClientIP == sess.ClientIP {
				n++
			}
		}
		if n >= lim.MaxPerIP {
			return "", ErrIPLimit
		}
	}

	app := ""
	if env.Kind == model.KindApplab {
		app = s.freeSlot(env)
		if app == "" {
			return "", ErrNoSlot
		}
	}

	s.sessions = append(s.sessions, sess)
	if app != "" {
		if s.slots[env.ID] == nil {
			s.slots[env.ID] = map[string]string{}
		}
		s.slots[env.ID][app] = sess.ID
	}
	return app, nil
}

// Complete records the address and details a driver minted for a session.
//
// The template comes back from the driver rather than from the request, because
// only the driver knows which one it settled on: a caller that named none gets
// the environment's own default, and the session must record what actually ran,
// not what was asked for.
func (s *Store) Complete(id, consoleURL, app, sandboxID, template string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.sessions {
		if s.sessions[i].ID != id {
			continue
		}
		s.sessions[i].ConsoleURL = consoleURL
		if app != "" {
			s.sessions[i].App = app
		}
		if sandboxID != "" {
			s.sessions[i].SandboxID = sandboxID
		}
		if template != "" {
			s.sessions[i].Template = template
		}
		return nil
	}
	return ErrNotFound
}

// Drop removes a session and frees any slot it held. It is idempotent: dropping
// a session that is already gone is success, because the caller's intent —
// "this session must not exist" — is satisfied either way, and the explicit
// delete and the reaper routinely race to be the one that drops it.
func (s *Store) Drop(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	kept := s.sessions[:0]
	for _, sess := range s.sessions {
		if sess.ID == id {
			found = true
			continue
		}
		kept = append(kept, sess)
	}
	if !found {
		return nil
	}
	s.sessions = kept
	for envID, slots := range s.slots {
		for app, sessID := range slots {
			if sessID == id {
				delete(slots, app)
			}
		}
		if len(slots) == 0 {
			delete(s.slots, envID)
		}
	}
	return nil
}

// freeSlot returns a free app id for an applab environment, or "" if none is
// free. Callers hold the lock.
func (s *Store) freeSlot(env model.Env) string {
	used := s.slots[env.ID]
	for _, app := range env.Slots {
		if _, taken := used[app]; !taken {
			return app
		}
	}
	return ""
}

// snapshot copies the session list for a caller to read outside the lock, newest
// first. Callers hold the lock.
func (s *Store) snapshot() []model.Session {
	out := make([]model.Session, len(s.sessions))
	copy(out, s.sessions)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
