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

	// ErrNoSlot is an environment with no capacity left: it is already carrying
	// as many concurrent sessions as it may.
	ErrNoSlot = errors.New("store: the environment is full")

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
}

// New returns an empty store.
func New() *Store {
	return &Store{}
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

// Reserve records the session, all under one lock, so that the limits it checks
// and the record it writes cannot be raced apart by two requests arriving
// together.
//
// Capacity is per environment and is a count, not a pool: an applab environment
// serves at most env.Capacity concurrent sessions, and the instance each one
// gets is minted by the driver afterwards. An environment with no room left is
// ErrNoSlot, which the caller reads as "try another environment of this kind".
//
// The session is recorded with whatever fields the caller has already filled in
// — id, env, address, times — and ConsoleURL empty, because the address is not
// known until the driver has minted the credential. Complete fills it in.
func (s *Store) Reserve(env model.Env, sess model.Session, lim Limits) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.sessions {
		if existing.ID == sess.ID {
			return ErrExists
		}
	}
	if lim.MaxTotal > 0 && len(s.sessions) >= lim.MaxTotal {
		return ErrAtCapacity
	}
	if lim.MaxPerIP > 0 {
		n := 0
		for _, existing := range s.sessions {
			if existing.ClientIP == sess.ClientIP {
				n++
			}
		}
		if n >= lim.MaxPerIP {
			return ErrIPLimit
		}
	}

	// Counted rather than looked up in a name pool: this environment's own
	// sessions are what it is carrying, and the check sits inside the same lock
	// as the append so two arrivals cannot both see room for the last place.
	// applab only — a sandboxlab environment is capped by the global limit
	// alone, which is what it has always been.
	if env.Kind == model.KindApplab && env.Capacity > 0 {
		n := 0
		for _, existing := range s.sessions {
			if existing.EnvID == env.ID {
				n++
			}
		}
		if n >= env.Capacity {
			return ErrNoSlot
		}
	}

	s.sessions = append(s.sessions, sess)
	return nil
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

// Drop removes a session. It is idempotent: dropping a session that is already
// gone is success, because the caller's intent — "this session must not exist" —
// is satisfied either way, and the explicit delete and the reaper routinely race
// to be the one that drops it.
//
// Removing it also gives the environment its capacity back, since capacity is
// counted from this list.
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
	return nil
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
