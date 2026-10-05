package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/shaowenchen/labs/internal/config"
	"github.com/shaowenchen/labs/internal/driver"
	"github.com/shaowenchen/labs/internal/model"
	"github.com/shaowenchen/labs/internal/store"
)

// fakeDriver records what it was asked to do, and can be told to fail.
type fakeDriver struct {
	live         []driver.Live
	liveErr      error
	mu           sync.Mutex
	provisioned  []string // app ids
	released     []string
	reconciled   int
	ready        bool
	provisionErr error
}

func (f *fakeDriver) Kind() model.Kind { return model.KindApplab }

func (f *fakeDriver) Ready(context.Context, model.Env) (driver.Ready, error) {
	if !f.ready {
		return driver.Ready{Message: "down"}, nil
	}
	return driver.Ready{Ready: true, ConsoleURL: "https://a.example.com/applab"}, nil
}

func (f *fakeDriver) Provision(_ context.Context, _ model.Env, req driver.ProvisionRequest) (driver.Provisioned, error) {
	if f.provisionErr != nil {
		return driver.Provisioned{}, f.provisionErr
	}
	f.mu.Lock()
	f.provisioned = append(f.provisioned, req.App)
	f.mu.Unlock()
	return driver.Provisioned{ConsoleURL: "https://a.example.com/applab", APIKey: "key-" + req.App, App: req.App}, nil
}

func (f *fakeDriver) Release(_ context.Context, _ model.Env, s model.Session) error {
	f.mu.Lock()
	f.released = append(f.released, s.App)
	f.mu.Unlock()
	return nil
}

func (f *fakeDriver) EnsureSlot(context.Context, model.Env, string) error { return nil }

func (f *fakeDriver) Live(context.Context, model.Env) ([]driver.Live, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live, f.liveErr
}

func (f *fakeDriver) Reconcile(context.Context, model.Env, []model.Session) error {
	f.mu.Lock()
	f.reconciled++
	f.mu.Unlock()
	return nil
}

func testManager(t *testing.T, drv driver.Driver, cfg config.Config) (*Manager, *store.Store) {
	t.Helper()
	st := store.New()
	m := New(cfg, st, map[model.Kind]driver.Driver{model.KindApplab: drv},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	// A frozen clock so expiry is deterministic.
	base := time.Now()
	m.WithClock(func() time.Time { return base })
	return m, st
}

func testConfig() config.Config {
	return config.Config{
		SessionTTL:       2 * time.Hour,
		MaxSessionsPerIP: 1,
		Envs: []model.Env{{
			ID: "applab-1", Kind: model.KindApplab, Domain: "a.example.com", BasePath: "/applab",
			Slots: []string{"lab-01", "lab-02"}, Capacity: 2,
		}},
	}
}

func TestProvisionDeliversASession(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, st := testManager(t, drv, testConfig())

	got, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got.APIKey != "key-lab-01" {
		t.Errorf("APIKey = %q", got.APIKey)
	}
	if got.ConsoleURL != "https://a.example.com/applab" {
		t.Errorf("ConsoleURL = %q", got.ConsoleURL)
	}
	if _, ok := st.Get(got.Session.ID); !ok {
		t.Error("the session was not recorded")
	}
	if got.Session.ExpiresAt.Sub(got.Session.CreatedAt) != 2*time.Hour {
		t.Errorf("session TTL = %s, want 2h", got.Session.ExpiresAt.Sub(got.Session.CreatedAt))
	}
}

func TestProvisionWithNoReadyEnvIsRetryable(t *testing.T) {
	drv := &fakeDriver{ready: false}
	m, _ := testManager(t, drv, testConfig())

	_, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1")
	if !errors.Is(err, ErrNoReadyEnv) {
		t.Fatalf("Provision = %v, want ErrNoReadyEnv", err)
	}
}

// Two callers must land on different slots, and the second slot must be free
// once the first is released.
func TestTwoSessionsTakeDifferentSlots(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())

	a, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Provision(context.Background(), model.KindApplab, "2.2.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if a.Session.App == b.Session.App {
		t.Fatalf("both sessions took slot %q", a.Session.App)
	}

	if err := m.Release(context.Background(), a.Session.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	c, err := m.Provision(context.Background(), model.KindApplab, "3.3.3.3")
	if err != nil {
		t.Fatal(err)
	}
	if c.Session.App != a.Session.App {
		t.Errorf("the freed slot %q was not reused; got %q", a.Session.App, c.Session.App)
	}
}

// If minting the credential fails, the reserved slot must be given back, or the
// environment would leak a slot per failed request until it looked full.
func TestProvisionRollsBackTheSlotOnFailure(t *testing.T) {
	drv := &fakeDriver{ready: true, provisionErr: errors.New("boom")}
	m, st := testManager(t, drv, testConfig())

	if _, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1"); err == nil {
		t.Fatal("Provision should have failed")
	}
	if st.Total() != 0 {
		t.Fatalf("a failed provision left %d sessions recorded", st.Total())
	}
	if got := st.OccupiedSlots("applab-1"); len(got) != 0 {
		t.Fatalf("a failed provision left slots held: %v", got)
	}
	// The slot must be usable again.
	drv.provisionErr = nil
	if _, err := m.Provision(context.Background(), model.KindApplab, "2.2.2.2"); err != nil {
		t.Fatalf("the slot was not recoverable: %v", err)
	}
}

func TestPerIPLimitIsEnforced(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())
	if _, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1")
	if !errors.Is(err, store.ErrIPLimit) {
		t.Fatalf("second session from one address = %v, want ErrIPLimit", err)
	}
}

func TestExpireReleasesAndCallsTheDriver(t *testing.T) {
	base := time.Now()
	drv := &fakeDriver{ready: true}
	st := store.New()
	m := New(testConfig(), st, map[model.Kind]driver.Driver{model.KindApplab: drv},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.WithClock(func() time.Time { return base })

	got, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}

	// Move the clock past the session's expiry.
	m.WithClock(func() time.Time { return base.Add(3 * time.Hour) })

	n, err := m.Expire(context.Background())
	if err != nil {
		t.Fatalf("Expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("Expire released %d sessions, want 1", n)
	}
	if _, ok := st.Get(got.Session.ID); ok {
		t.Error("the expired session is still recorded")
	}
	if len(drv.released) != 1 {
		t.Fatalf("the driver was asked to release %d times, want 1", len(drv.released))
	}
}

func TestUnknownKindIsRefused(t *testing.T) {
	m, _ := testManager(t, &fakeDriver{ready: true}, testConfig())
	if _, err := m.Provision(context.Background(), model.Kind("nope"), "1.1.1.1"); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Provision = %v, want ErrUnknownKind", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())
	got, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Release(context.Background(), got.Session.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(context.Background(), got.Session.ID); err != nil {
		t.Fatalf("second release should be a no-op, got %v", err)
	}
}

func TestNewIDIsUnguessableLength(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewID()
		if len(id) != 32 {
			t.Fatalf("NewID() = %q, want 32 hex characters", id)
		}
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
}

func TestEqualID(t *testing.T) {
	if !EqualID("abc", "abc") {
		t.Error("equal ids should compare equal")
	}
	if EqualID("abc", "abd") {
		t.Error("different ids should not compare equal")
	}
}

// With nothing up and a way to start one, a provision starts an environment —
// this is what lets a serverless host, where a request is the only thing that
// runs, bring an environment up by asking for a lab. The request itself cannot
// wait the minutes it takes to boot, so it answers "not yet" and the environment
// it started is there for the next one.
func TestProvisionStartsAnEnvironmentWhenNoneIsUp(t *testing.T) {
	drv := &fakeDriver{ready: false}
	st := store.New()
	m := New(testConfig(), st, map[model.Kind]driver.Driver{model.KindApplab: drv},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A clock the test can advance, so the readiness cache (15s) is past by the
	// second request.
	now := time.Now()
	m.WithClock(func() time.Time { return now })

	var started []string
	m.WithStarter(func(_ context.Context, env model.Env) bool {
		started = append(started, env.ID)
		drv.ready = true // the start brought the environment up
		return true
	})

	_, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1")
	if !errors.Is(err, ErrNoReadyEnv) {
		t.Fatalf("Provision = %v, want ErrNoReadyEnv (the environment is still booting)", err)
	}
	if len(started) != 1 {
		t.Fatalf("expected one start attempt, got %v", started)
	}

	// The next request, once the environment is up, gets a lab.
	now = now.Add(time.Minute)
	got, err := m.Provision(context.Background(), model.KindApplab, "2.2.2.2")
	if err != nil {
		t.Fatalf("Provision after the environment came up: %v", err)
	}
	if got.APIKey == "" {
		t.Error("no lab was delivered after starting the environment")
	}
}

// With nothing up and no way to start one, the answer is "nothing is up", not a
// delivery — and it says so without claiming a start.
func TestProvisionWithoutAStarterReportsNoEnvironment(t *testing.T) {
	drv := &fakeDriver{ready: false}
	m, _ := testManager(t, drv, testConfig())

	_, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1")
	if !errors.Is(err, ErrNoReadyEnv) {
		t.Fatalf("Provision = %v, want ErrNoReadyEnv", err)
	}
}

// Occupied is counted from what the environment is actually running, not from
// this service's records — so a restart does not make a busy cluster read as
// empty. The driver's list is the source; the store is only the fallback for an
// environment that cannot be read.
func TestStatusCountsWhatTheEnvironmentIsRunning(t *testing.T) {
	drv := &fakeDriver{ready: true, live: []driver.Live{{ID: "lab-01"}, {ID: "lab-02"}}}
	m, _ := testManager(t, drv, testConfig())

	// Nothing has been provisioned by this process — as after a restart — yet
	// the environment reports two instances, so that is the count.
	got := m.Status(context.Background())
	if len(got) != 1 || got[0].Occupied != 2 {
		t.Fatalf("occupied = %v, want 2 from the environment's own list", got)
	}
}

// An environment that cannot be read falls back to this service's record,
// because a stale number beats a zero that looks like an empty cluster.
func TestStatusFallsBackToTheRecordedCount(t *testing.T) {
	drv := &fakeDriver{ready: true, liveErr: errors.New("unreachable")}
	m, _ := testManager(t, drv, testConfig())

	if _, err := m.Provision(context.Background(), model.KindApplab, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}

	got := m.Status(context.Background())
	if len(got) != 1 || got[0].Occupied != 1 {
		t.Fatalf("occupied = %v, want 1 from the recorded slot", got)
	}
}

// The run check comes before the service: an environment with no run is
// reported as such, and its address is never probed — a probe would only return
// a tunnel error that says less.
func TestNoRunIsReportedWithoutProbingTheService(t *testing.T) {
	drv := &fakeDriver{ready: true} // the address would answer if it were asked
	m, _ := testManager(t, drv, testConfig())
	m.WithRunnerCheck(func(context.Context, model.Env) (bool, string, bool) {
		return false, "no run is active", true
	})

	got := m.Status(context.Background())
	if len(got) != 1 {
		t.Fatalf("Status = %v", got)
	}
	if got[0].Ready {
		t.Error("an environment with no run should not read as ready")
	}
	if got[0].Message != "no run is active" {
		t.Errorf("Message = %q, want the run check's reason", got[0].Message)
	}
}

// When the run check cannot answer — GitHub refused the listing — the service
// is still probed, because the environment may well be up and only the listing
// broken. A check that cannot tell must not take an environment out of service.
func TestUnknownRunFallsThroughToTheProbe(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())
	m.WithRunnerCheck(func(context.Context, model.Env) (bool, string, bool) {
		return false, "", false // unknown
	})

	got := m.Status(context.Background())
	if len(got) != 1 || !got[0].Ready {
		t.Fatalf("Status = %v, want the probe's verdict (ready)", got)
	}
}

// Status gates on the run: with no run, the environment reports why and is not
// asked anything else — no ready probe, no instance count. That is what keeps a
// stopped environment from answering with a tunnel error.
func TestStatusGatesOnTheRun(t *testing.T) {
	drv := &fakeDriver{ready: true, live: []driver.Live{{ID: "lab-01"}}}
	m, _ := testManager(t, drv, testConfig())
	m.WithRunnerCheck(func(context.Context, model.Env) (bool, string, bool) {
		return false, "no run of debugger.yml is active", true
	})

	got := m.Status(context.Background())
	if len(got) != 1 {
		t.Fatalf("Status = %v", got)
	}
	st := got[0]
	if st.Ready || st.Occupied != 0 {
		t.Errorf("with no run: ready=%v occupied=%d, want false/0", st.Ready, st.Occupied)
	}
	if st.Message == "" {
		t.Error("with no run, the status should say so")
	}
}
