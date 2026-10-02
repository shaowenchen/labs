package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaowenchen/labs/internal/model"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "labs.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func applabEnv() model.Env {
	return model.Env{
		ID:       "applab-1",
		Kind:     model.KindApplab,
		Slots:    []string{"lab-01", "lab-02"},
		Capacity: 2,
	}
}

func aSession(id, ip string) model.Session {
	now := time.Now()
	return model.Session{ID: id, EnvID: "applab-1", Kind: model.KindApplab, ClientIP: ip, CreatedAt: now, ExpiresAt: now.Add(2 * time.Hour)}
}

func TestReserveClaimsDistinctSlots(t *testing.T) {
	s := tempStore(t)
	env := applabEnv()

	a, err := s.Reserve(env, aSession("s1", "1.1.1.1"), Limits{})
	if err != nil {
		t.Fatalf("Reserve s1: %v", err)
	}
	b, err := s.Reserve(env, aSession("s2", "2.2.2.2"), Limits{})
	if err != nil {
		t.Fatalf("Reserve s2: %v", err)
	}
	if a == b {
		t.Fatalf("two sessions got the same slot %q", a)
	}
	if a != "lab-01" || b != "lab-02" {
		t.Errorf("slots = %q, %q; want lab-01, lab-02", a, b)
	}
}

func TestReserveRefusesWhenSlotsAreFull(t *testing.T) {
	s := tempStore(t)
	env := applabEnv()
	mustReserve(t, s, env, "s1", "1.1.1.1")
	mustReserve(t, s, env, "s2", "2.2.2.2")

	if _, err := s.Reserve(env, aSession("s3", "3.3.3.3"), Limits{}); err != ErrNoSlot {
		t.Fatalf("Reserve on a full environment = %v, want ErrNoSlot", err)
	}
}

func TestReserveEnforcesPerIPLimit(t *testing.T) {
	s := tempStore(t)
	env := applabEnv()
	mustReserve(t, s, env, "s1", "9.9.9.9")

	_, err := s.Reserve(env, aSession("s2", "9.9.9.9"), Limits{MaxPerIP: 1})
	if err != ErrIPLimit {
		t.Fatalf("second session from one address = %v, want ErrIPLimit", err)
	}
}

func TestDropFreesTheSlot(t *testing.T) {
	s := tempStore(t)
	env := applabEnv()
	app := mustReserve(t, s, env, "s1", "1.1.1.1")

	if err := s.Drop("s1"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if got := s.OccupiedSlots("applab-1"); len(got) != 0 {
		t.Fatalf("slots still held after drop: %v", got)
	}
	// The freed slot must be handed out again.
	again := mustReserve(t, s, env, "s2", "2.2.2.2")
	if again != app {
		t.Errorf("freed slot %q was not reused; got %q", app, again)
	}
}

func TestDropIsIdempotent(t *testing.T) {
	s := tempStore(t)
	mustReserve(t, s, applabEnv(), "s1", "1.1.1.1")
	if err := s.Drop("s1"); err != nil {
		t.Fatalf("first Drop: %v", err)
	}
	if err := s.Drop("s1"); err != nil {
		t.Fatalf("second Drop should be a no-op, got %v", err)
	}
}

func TestExpiredFindsOnlyPastSessions(t *testing.T) {
	s := tempStore(t)
	env := applabEnv()
	now := time.Now()
	past := model.Session{ID: "old", EnvID: "applab-1", Kind: model.KindApplab, CreatedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	future := model.Session{ID: "new", EnvID: "applab-1", Kind: model.KindApplab, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if _, err := s.Reserve(env, past, Limits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve(env, future, Limits{}); err != nil {
		t.Fatal(err)
	}

	got := s.Expired(now)
	if len(got) != 1 || got[0].ID != "old" {
		t.Fatalf("Expired = %v, want just the old session", got)
	}
}

// The state must survive a restart: this is the whole reason it is on disk.
func TestStateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mustReserve(t, s, applabEnv(), "s1", "1.1.1.1")

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.Get("s1"); !ok {
		t.Fatal("the session did not survive a reopen")
	}
	if got := reopened.OccupiedSlots("applab-1"); got["lab-01"] != "s1" {
		t.Fatalf("slot occupancy did not survive a reopen: %v", got)
	}
}

// A file that cannot be parsed must fail loudly rather than starting empty: an
// empty start would forget live sessions and leave their credentials unexpired.
func TestAParseErrorIsNotASilentReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labs.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a corrupt state file")
	}
}

// The key must never be serialised: a leaked state file must not be a leaked
// credential. Session has no key field, and this guards against one being added.
func TestStateCarriesNoCredential(t *testing.T) {
	s := tempStore(t)
	app := mustReserve(t, s, applabEnv(), "s1", "1.1.1.1")
	if err := s.Complete("s1", "https://a.example.com/applab", app, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	blob := string(raw)
	for _, banned := range []string{"api_key", "user-key", "admin-key"} {
		if contains(blob, banned) {
			t.Errorf("state file contains %q, which it must not", banned)
		}
	}
}

func mustReserve(t *testing.T, s *Store, env model.Env, id, ip string) string {
	t.Helper()
	app, err := s.Reserve(env, aSession(id, ip), Limits{})
	if err != nil {
		t.Fatalf("Reserve %s: %v", id, err)
	}
	return app
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
