package store

import (
	"testing"
	"time"

	"github.com/shaowenchen/labs/internal/model"
)

func applabEnv() model.Env {
	return model.Env{
		ID:       "applab-1",
		Kind:     model.KindApplab,
		Capacity: 2,
	}
}

func aSession(id, ip string) model.Session {
	now := time.Now()
	return model.Session{ID: id, EnvID: "applab-1", Kind: model.KindApplab, ClientIP: ip, CreatedAt: now, ExpiresAt: now.Add(2 * time.Hour)}
}

// Capacity is a count, not a set of names: as many sessions as the environment
// allows are recorded, and the instances they get are minted later by the driver.
func TestReserveRecordsUpToCapacity(t *testing.T) {
	s := New()
	env := applabEnv()

	mustReserve(t, s, env, "s1", "1.1.1.1")
	mustReserve(t, s, env, "s2", "2.2.2.2")

	if got := s.Total(); got != 2 {
		t.Errorf("Total = %d, want 2 recorded", got)
	}
}

func TestReserveRefusesWhenTheEnvironmentIsFull(t *testing.T) {
	s := New()
	env := applabEnv()
	mustReserve(t, s, env, "s1", "1.1.1.1")
	mustReserve(t, s, env, "s2", "2.2.2.2")

	if err := s.Reserve(env, aSession("s3", "3.3.3.3"), Limits{}); err != ErrNoSlot {
		t.Fatalf("Reserve on a full environment = %v, want ErrNoSlot", err)
	}
}

// Capacity is per environment: a second one of the same kind has its own places,
// so filling one does not stop the other.
func TestReserveCountsCapacityPerEnvironment(t *testing.T) {
	s := New()
	full := applabEnv()
	mustReserve(t, s, full, "s1", "1.1.1.1")
	mustReserve(t, s, full, "s2", "2.2.2.2")

	other := full
	other.ID = "applab-2"
	if err := s.Reserve(other, model.Session{ID: "s3", EnvID: "applab-2", Kind: model.KindApplab, ClientIP: "3.3.3.3"}, Limits{}); err != nil {
		t.Fatalf("a second environment should have its own capacity: %v", err)
	}
}

// sandboxlab is not capped per environment, which is what it has always been:
// only the global and per-address limits apply.
func TestReserveDoesNotCapSandboxlabPerEnvironment(t *testing.T) {
	s := New()
	env := model.Env{ID: "sandboxlab-1", Kind: model.KindSandboxlab, Capacity: 1}

	for i, id := range []string{"s1", "s2", "s3"} {
		sess := model.Session{ID: id, EnvID: env.ID, Kind: model.KindSandboxlab, ClientIP: "9.9.9." + string(rune('1'+i))}
		if err := s.Reserve(env, sess, Limits{}); err != nil {
			t.Fatalf("Reserve %s = %v, want no per-environment cap for sandboxlab", id, err)
		}
	}
}

func TestReserveEnforcesPerIPLimit(t *testing.T) {
	s := New()
	env := applabEnv()
	mustReserve(t, s, env, "s1", "9.9.9.9")

	err := s.Reserve(env, aSession("s2", "9.9.9.9"), Limits{MaxPerIP: 1})
	if err != ErrIPLimit {
		t.Fatalf("second session from one address = %v, want ErrIPLimit", err)
	}
}

func TestReserveEnforcesTheGlobalLimit(t *testing.T) {
	s := New()
	env := applabEnv()
	mustReserve(t, s, env, "s1", "1.1.1.1")

	if err := s.Reserve(applabEnv(), aSession("s2", "2.2.2.2"), Limits{MaxTotal: 1}); err != ErrAtCapacity {
		t.Fatalf("over the global cap = %v, want ErrAtCapacity", err)
	}
}

// Dropping a session gives the environment its place back, so the next caller
// fits where the last one was.
func TestDropFreesACapacityPlace(t *testing.T) {
	s := New()
	env := applabEnv()
	mustReserve(t, s, env, "s1", "1.1.1.1")
	mustReserve(t, s, env, "s2", "2.2.2.2")

	if err := s.Drop("s1"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if err := s.Reserve(env, aSession("s3", "3.3.3.3"), Limits{}); err != nil {
		t.Fatalf("the freed place should be usable: %v", err)
	}
}

func TestDropIsIdempotent(t *testing.T) {
	s := New()
	mustReserve(t, s, applabEnv(), "s1", "1.1.1.1")
	if err := s.Drop("s1"); err != nil {
		t.Fatalf("first Drop: %v", err)
	}
	if err := s.Drop("s1"); err != nil {
		t.Fatalf("second Drop should be a no-op, got %v", err)
	}
}

func TestExpiredFindsOnlyPastSessions(t *testing.T) {
	s := New()
	env := applabEnv()
	now := time.Now()
	past := model.Session{ID: "old", EnvID: "applab-1", Kind: model.KindApplab, CreatedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	future := model.Session{ID: "new", EnvID: "applab-1", Kind: model.KindApplab, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.Reserve(env, past, Limits{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(env, future, Limits{}); err != nil {
		t.Fatal(err)
	}

	got := s.Expired(now)
	if len(got) != 1 || got[0].ID != "old" {
		t.Fatalf("Expired = %v, want just the old session", got)
	}
}

func TestCompleteRecordsTheAddress(t *testing.T) {
	s := New()
	mustReserve(t, s, applabEnv(), "s1", "1.1.1.1")
	if err := s.Complete("s1", "https://a.example.com/applab", "", "", ""); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("session is gone after Complete")
	}
	if got.ConsoleURL != "https://a.example.com/applab" {
		t.Errorf("ConsoleURL = %q", got.ConsoleURL)
	}
	// Complete records the app id the driver minted, which is now its own name
	// rather than something Reserve picked.
	if err := s.Complete("s1", "https://a.example.com/applab", "lab-abcdef12", "", ""); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	if again, _ := s.Get("s1"); again.App != "lab-abcdef12" {
		t.Errorf("App = %q, want the driver's app id", again.App)
	}
}

func TestCompleteOnAnUnknownSessionIsNotFound(t *testing.T) {
	if err := New().Complete("nope", "u", "", "", ""); err != ErrNotFound {
		t.Fatalf("Complete on an unknown session = %v, want ErrNotFound", err)
	}
}

func TestSessionsAreNewestFirst(t *testing.T) {
	s := New()
	now := time.Now()
	env := applabEnv()
	env.Capacity = 3
	for i, at := range []time.Time{now.Add(-2 * time.Minute), now, now.Add(-time.Minute)} {
		sess := model.Session{ID: string(rune('a' + i)), EnvID: "applab-1", Kind: model.KindApplab, CreatedAt: at, ExpiresAt: at.Add(time.Hour)}
		if err := s.Reserve(env, sess, Limits{}); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Sessions()
	if len(got) != 3 || !got[0].CreatedAt.Equal(now) {
		t.Fatalf("Sessions not newest first: %+v", got)
	}
}

// mustReserve records a session and fails the test if the environment refuses it.
func mustReserve(t *testing.T, s *Store, env model.Env, id, ip string) {
	t.Helper()
	if err := s.Reserve(env, aSession(id, ip), Limits{}); err != nil {
		t.Fatalf("Reserve %s: %v", id, err)
	}
}
