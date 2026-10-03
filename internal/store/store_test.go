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
		Slots:    []string{"lab-01", "lab-02"},
		Capacity: 2,
	}
}

func aSession(id, ip string) model.Session {
	now := time.Now()
	return model.Session{ID: id, EnvID: "applab-1", Kind: model.KindApplab, ClientIP: ip, CreatedAt: now, ExpiresAt: now.Add(2 * time.Hour)}
}

func TestReserveClaimsDistinctSlots(t *testing.T) {
	s := New()
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
	s := New()
	env := applabEnv()
	mustReserve(t, s, env, "s1", "1.1.1.1")
	mustReserve(t, s, env, "s2", "2.2.2.2")

	if _, err := s.Reserve(env, aSession("s3", "3.3.3.3"), Limits{}); err != ErrNoSlot {
		t.Fatalf("Reserve on a full environment = %v, want ErrNoSlot", err)
	}
}

func TestReserveEnforcesPerIPLimit(t *testing.T) {
	s := New()
	env := applabEnv()
	mustReserve(t, s, env, "s1", "9.9.9.9")

	_, err := s.Reserve(env, aSession("s2", "9.9.9.9"), Limits{MaxPerIP: 1})
	if err != ErrIPLimit {
		t.Fatalf("second session from one address = %v, want ErrIPLimit", err)
	}
}

func TestReserveEnforcesTheGlobalLimit(t *testing.T) {
	s := New()
	env := applabEnv()
	mustReserve(t, s, env, "s1", "1.1.1.1")

	_, err := s.Reserve(applabEnv(), aSession("s2", "2.2.2.2"), Limits{MaxTotal: 1})
	if err != ErrAtCapacity {
		t.Fatalf("over the global cap = %v, want ErrAtCapacity", err)
	}
}

func TestDropFreesTheSlot(t *testing.T) {
	s := New()
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

func TestCompleteRecordsTheAddress(t *testing.T) {
	s := New()
	app := mustReserve(t, s, applabEnv(), "s1", "1.1.1.1")
	if err := s.Complete("s1", "https://a.example.com/applab", app, ""); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("session is gone after Complete")
	}
	if got.ConsoleURL != "https://a.example.com/applab" {
		t.Errorf("ConsoleURL = %q", got.ConsoleURL)
	}
	if got.App != app {
		t.Errorf("App = %q, want %q", got.App, app)
	}
}

func TestCompleteOnAnUnknownSessionIsNotFound(t *testing.T) {
	if err := New().Complete("nope", "u", "", ""); err != ErrNotFound {
		t.Fatalf("Complete on an unknown session = %v, want ErrNotFound", err)
	}
}

func TestSessionsAreNewestFirst(t *testing.T) {
	s := New()
	now := time.Now()
	env := applabEnv()
	env.Slots = []string{"lab-01", "lab-02", "lab-03"}
	env.Capacity = 3
	for i, at := range []time.Time{now.Add(-2 * time.Minute), now, now.Add(-time.Minute)} {
		sess := model.Session{ID: string(rune('a' + i)), EnvID: "applab-1", Kind: model.KindApplab, CreatedAt: at, ExpiresAt: at.Add(time.Hour)}
		if _, err := s.Reserve(env, sess, Limits{}); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Sessions()
	if len(got) != 3 || !got[0].CreatedAt.Equal(now) {
		t.Fatalf("Sessions not newest first: %+v", got)
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
