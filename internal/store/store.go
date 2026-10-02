// Package store records the sessions this service has handed out.
//
// It is one JSON file, rewritten whole on every change, guarded by a mutex. That
// is enough for a single process and it is deliberately not more: the service
// runs one replica, because the file and the rate limiter are both per-process.
// A second replica would need a shared store and a distributed limiter, and
// pretending this file is safe to share would be the worse answer.
//
// The file is not a place for secrets. A session's API key is used once, when it
// is minted, and expiry is enforced by rotating the credential rather than by
// re-reading it — so the key is never written here, and a leaked state file is
// not a leaked credential.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// Store is the session record.
type Store struct {
	mu   sync.Mutex
	path string
	st   state
}

type state struct {
	Version  int             `json:"version"`
	Sessions []model.Session `json:"sessions"`

	// Slots maps an environment id to its app ids and the session occupying
	// each, so slot occupancy and the session list cannot disagree: they are in
	// one file, written in one transaction.
	Slots map[string]map[string]string `json:"slots"`
}

const stateVersion = 1

// Open loads the store from path, creating an empty one if the file is absent.
//
// A file that exists but cannot be parsed is an error rather than a fresh
// start: silently forgetting every live session would leave the credentials it
// handed out unexpired and unaccounted for, which is the one failure this file
// exists to prevent.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: no path given")
	}
	s := &Store{
		path: path,
		st:   state{Version: stateVersion, Slots: map[string]map[string]string{}},
	}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, fmt.Errorf("store: parse %s: %w", path, err)
		}
		if s.st.Slots == nil {
			s.st.Slots = map[string]map[string]string{}
		}
	case os.IsNotExist(err):
		// Nothing to load. The directory is created lazily on first write so a
		// read-only mount still starts, and only fails when it must record.
	default:
		return nil, fmt.Errorf("store: read %s: %w", path, err)
	}
	return s, nil
}

// Path is the file this store writes.
func (s *Store) Path() string { return s.path }

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
	for _, sess := range s.st.Sessions {
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
	for _, sess := range s.st.Sessions {
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
	for _, sess := range s.st.Sessions {
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
	return len(s.st.Sessions)
}

// OccupiedSlots returns the app ids an applab environment currently has in use,
// mapped to the session holding each. It is what Reconcile compares against.
func (s *Store) OccupiedSlots(envID string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for app, sessID := range s.st.Slots[envID] {
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

	for _, existing := range s.st.Sessions {
		if existing.ID == sess.ID {
			return "", ErrExists
		}
	}
	if lim.MaxTotal > 0 && len(s.st.Sessions) >= lim.MaxTotal {
		return "", ErrAtCapacity
	}
	if lim.MaxPerIP > 0 {
		n := 0
		for _, existing := range s.st.Sessions {
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

	s.st.Sessions = append(s.st.Sessions, sess)
	if app != "" {
		if s.st.Slots[env.ID] == nil {
			s.st.Slots[env.ID] = map[string]string{}
		}
		s.st.Slots[env.ID][app] = sess.ID
	}
	if err := s.persist(); err != nil {
		// Roll back so memory and disk agree. Without this a failed write would
		// leave the slot claimed in memory but absent from the file, and a
		// restart would hand it out twice.
		s.st.Sessions = s.st.Sessions[:len(s.st.Sessions)-1]
		if app != "" {
			delete(s.st.Slots[env.ID], app)
		}
		return "", err
	}
	return app, nil
}

// Complete records the address and details a driver minted for a session.
func (s *Store) Complete(id, consoleURL, app, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Sessions {
		if s.st.Sessions[i].ID != id {
			continue
		}
		s.st.Sessions[i].ConsoleURL = consoleURL
		if app != "" {
			s.st.Sessions[i].App = app
		}
		if sandboxID != "" {
			s.st.Sessions[i].SandboxID = sandboxID
		}
		return s.persist()
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
	kept := s.st.Sessions[:0]
	for _, sess := range s.st.Sessions {
		if sess.ID == id {
			found = true
			continue
		}
		kept = append(kept, sess)
	}
	if !found {
		return nil
	}
	s.st.Sessions = kept
	for envID, slots := range s.st.Slots {
		for app, sessID := range slots {
			if sessID == id {
				delete(slots, app)
			}
		}
		if len(slots) == 0 {
			delete(s.st.Slots, envID)
		}
	}
	return s.persist()
}

// freeSlot returns a free app id for an applab environment, or "" if none is
// free. Callers hold the lock.
func (s *Store) freeSlot(env model.Env) string {
	used := s.st.Slots[env.ID]
	for _, app := range env.Slots {
		if _, taken := used[app]; !taken {
			return app
		}
	}
	return ""
}

// snapshot copies the session list for a caller to read outside the lock.
// Callers hold the lock.
func (s *Store) snapshot() []model.Session {
	out := make([]model.Session, len(s.st.Sessions))
	copy(out, s.st.Sessions)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// persist writes the whole state atomically. Callers hold the lock.
//
// The temp-file-and-rename dance is what makes the file never half-written: a
// reader either sees the previous state or the next one, never a truncation. The
// fsync before the rename is what makes that true across a crash, and the fsync
// of the directory is what makes the rename itself durable.
func (s *Store) persist() error {
	data, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".labs-*.tmp")
	if err != nil {
		return fmt.Errorf("store: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has moved it

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("store: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("store: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("store: rename onto %s: %w", s.path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
