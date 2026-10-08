package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
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
	templates    []string // templates passed to Provision
	released     []string
	reconciled   int
	ready        bool
	provisionErr error
	choices      []driver.Choice
	choicesErr   error
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
	// Mint the app id the way applab does: from the session id, so each session
	// gets its own name and nothing is reused.
	app := "lab-" + req.SessionID[:8]
	f.mu.Lock()
	f.provisioned = append(f.provisioned, app)
	f.templates = append(f.templates, req.Template)
	f.mu.Unlock()
	// Echo the template back the way sandboxlab does, so a test can tell the
	// session recorded what ran rather than what was asked for.
	return driver.Provisioned{
		ConsoleURL: "https://a.example.com/applab",
		APIKey:     "key-" + app,
		App:        app,
		Template:   req.Template,
	}, nil
}

func (f *fakeDriver) Release(_ context.Context, _ model.Env, s model.Session) error {
	f.mu.Lock()
	f.released = append(f.released, s.App)
	f.mu.Unlock()
	return nil
}

func (f *fakeDriver) Choices(context.Context, model.Env) ([]driver.Choice, error) {
	return f.choices, f.choicesErr
}

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
			Capacity: 2,
		}},
	}
}

func TestProvisionDeliversASession(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, st := testManager(t, drv, testConfig())

	got, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got.APIKey[:4] != "key-" {
		t.Errorf("APIKey = %q, want the key the driver minted", got.APIKey)
	}
	if got.Session.App == "" {
		t.Error("the session did not record the app the driver minted")
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

	_, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
	if !errors.Is(err, ErrNoReadyEnv) {
		t.Fatalf("Provision = %v, want ErrNoReadyEnv", err)
	}
}

// Two callers get their own app, and a released one is never handed out again:
// each session's app is minted from its own id, so nothing is reused.
func TestTwoSessionsGetTheirOwnApp(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())

	a, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "2.2.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if a.Session.App == "" || b.Session.App == "" {
		t.Fatalf("a session has no app: %q, %q", a.Session.App, b.Session.App)
	}
	if a.Session.App == b.Session.App {
		t.Fatalf("both sessions were given the app %q", a.Session.App)
	}

	if err := m.Release(context.Background(), a.Session.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	c, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "3.3.3.3")
	if err != nil {
		t.Fatal(err)
	}
	// The released app's name must not come back: it was deleted with the
	// session, and a name is never handed out twice.
	if c.Session.App == a.Session.App {
		t.Errorf("the released app %q was handed out again", a.Session.App)
	}
}

// If minting the credential fails, the recorded session must be given back, or
// the environment would leak a place per failed request until it looked full.
func TestProvisionRollsBackThePlaceOnFailure(t *testing.T) {
	drv := &fakeDriver{ready: true, provisionErr: errors.New("boom")}
	m, st := testManager(t, drv, testConfig())

	if _, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1"); err == nil {
		t.Fatal("Provision should have failed")
	}
	if st.Total() != 0 {
		t.Fatalf("a failed provision left %d sessions recorded", st.Total())
	}
	// The place must be usable again.
	drv.provisionErr = nil
	if _, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "2.2.2.2"); err != nil {
		t.Fatalf("the place was not recoverable: %v", err)
	}
}

func TestPerIPLimitIsEnforced(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())
	if _, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
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

	got, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
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
	if _, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.Kind("nope")}, "1.1.1.1"); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Provision = %v, want ErrUnknownKind", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())
	got, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
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

	_, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
	if !errors.Is(err, ErrNoReadyEnv) {
		t.Fatalf("Provision = %v, want ErrNoReadyEnv (the environment is still booting)", err)
	}
	if len(started) != 1 {
		t.Fatalf("expected one start attempt, got %v", started)
	}

	// The next request, once the environment is up, gets a lab.
	now = now.Add(time.Minute)
	got, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "2.2.2.2")
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

	_, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
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

	if _, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}

	got := m.Status(context.Background())
	if len(got) != 1 || got[0].Occupied != 1 {
		t.Fatalf("occupied = %v, want 1 from the recorded slot", got)
	}
}

// A lab that has just been handed out counts, even though the environment has
// nothing to report for it.
//
// This is the case the page was getting wrong: an applab slot becomes real only
// when the caller pushes something into it, so a slot a session holds is not in
// the environment's list at all. Counting that list alone made a lab read as
// zero from the moment it was created until whatever the caller deployed
// appeared — which is exactly when someone is looking at the page.
// The count and the list are the environment's own, so they agree even for a
// slot whose app is recorded but not yet deployed — the state right after a lab
// is handed out, when the caller most wants to see that it is there.
func TestStatusCountsWhatTheEnvironmentLists(t *testing.T) {
	drv := &fakeDriver{ready: true, live: []driver.Live{{ID: "lab-01", State: "created"}}}
	m, _ := testManager(t, drv, testConfig())

	got, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got.Session.App == "" {
		t.Fatal("Provision gave the session no slot")
	}

	status := m.Status(context.Background())
	if len(status) != 1 || status[0].Occupied != 1 {
		t.Fatalf("occupied = %v, want 1 from the environment's list", status)
	}

	live := m.Live(context.Background())
	if len(live) != 1 || len(live[0].Items) != 1 {
		t.Fatalf("Live = %+v, want the environment's one row", live)
	}
	if row := live[0].Items[0]; row.ID != "lab-01" || row.State != "created" {
		t.Errorf("row = %+v, want the environment's own row", row)
	}
}

// The environment cannot be read, so the service's own record is the only
// evidence there is — better a stale number than a zero that looks empty.
func TestStatusFallsBackToTheRecordWhenTheEnvironmentIsUnreadable(t *testing.T) {
	drv := &fakeDriver{ready: true, liveErr: errors.New("unreachable")}
	m, _ := testManager(t, drv, testConfig())

	if _, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1"); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	status := m.Status(context.Background())
	if len(status) != 1 || status[0].Occupied != 1 {
		t.Fatalf("occupied = %v, want the slot the record holds", status)
	}
}

// A slot a session holds and the environment also reports is one slot, not two:
// the count is a set, so a deployed lab is not double-counted once its app turns
// up in the environment's list.
func TestStatusCountsAHeldSlotTheEnvironmentAlsoReportsOnce(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())

	got, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	// The caller has now deployed something, so the environment reports the same
	// slot the session holds.
	drv.mu.Lock()
	drv.live = []driver.Live{{ID: got.Session.App, State: "running"}}
	drv.mu.Unlock()

	status := m.Status(context.Background())
	if len(status) != 1 || status[0].Occupied != 1 {
		t.Fatalf("occupied = %v, want 1 (the slot counted once)", status)
	}
	live := m.Live(context.Background())
	if len(live[0].Items) != 1 {
		t.Fatalf("Live = %+v, want one row", live)
	}
	// The environment's own state wins over the placeholder, so the row stops
	// saying "held" once there is something real behind it.
	if live[0].Items[0].State != "running" {
		t.Errorf("row state = %q, want the environment's %q", live[0].Items[0].State, "running")
	}
}

// A lab counts once it is gone from the service's records too — being released
// drops it from the count, so the number tracks what is actually held.
func TestStatusDropsAReleasedSlot(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())

	got, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Release(context.Background(), got.Session.ID); err != nil {
		t.Fatal(err)
	}

	status := m.Status(context.Background())
	if len(status) != 1 || status[0].Occupied != 0 {
		t.Fatalf("occupied = %v, want 0 after the only lab was released", status)
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
		return false, "the environment is not running yet", true
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

// A template the caller names is passed to the driver and recorded on the
// session, so a lab made from a choice can be told apart from the default.
func TestProvisionPassesAndRecordsTheTemplate(t *testing.T) {
	drv := &fakeDriver{ready: true, choices: []driver.Choice{{ID: "e2b"}, {ID: "agent-infra"}}}
	m, _ := testManager(t, drv, testConfig())

	got, err := m.Provision(context.Background(),
		ProvisionRequest{Kind: model.KindApplab, Template: "e2b"}, "1.1.1.1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(drv.templates) != 1 || drv.templates[0] != "e2b" {
		t.Errorf("driver saw templates %v, want [e2b]", drv.templates)
	}
	if got.Session.Template != "e2b" {
		t.Errorf("session template = %q, want e2b", got.Session.Template)
	}
}

// A template this kind does not offer is refused before anything is claimed: no
// slot is reserved and no session is left behind, so a typo costs a request
// rather than capacity.
func TestProvisionRefusesATemplateThatIsNotOffered(t *testing.T) {
	drv := &fakeDriver{ready: true, choices: []driver.Choice{{ID: "e2b"}}}
	m, st := testManager(t, drv, testConfig())

	_, err := m.Provision(context.Background(),
		ProvisionRequest{Kind: model.KindApplab, Template: "nope"}, "1.1.1.1")
	if !errors.Is(err, ErrNoSuchTemplate) {
		t.Fatalf("Provision = %v, want ErrNoSuchTemplate", err)
	}
	// The refusal names what does exist, so the next request can succeed.
	if !strings.Contains(err.Error(), "e2b") {
		t.Errorf("the refusal should name the available templates, got %q", err)
	}
	if n := len(drv.provisioned); n != 0 {
		t.Errorf("driver was asked to provision %d times, want 0", n)
	}
	if sessions := st.Sessions(); len(sessions) != 0 {
		t.Errorf("a refused request left %d sessions behind", len(sessions))
	}
	// The slot the request would have taken is still free for the next caller.
	if _, err := m.Provision(context.Background(),
		ProvisionRequest{Kind: model.KindApplab, Template: "e2b"}, "1.1.1.1"); err != nil {
		t.Fatalf("a good request after a refused one: %v", err)
	}
}

// A kind that offers no choice takes a lab with no template, and nothing is
// asked of the environment to resolve one.
func TestProvisionWithNoTemplateIsUntouched(t *testing.T) {
	drv := &fakeDriver{ready: true}
	m, _ := testManager(t, drv, testConfig())

	got, err := m.Provision(context.Background(), ProvisionRequest{Kind: model.KindApplab}, "1.1.1.1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got.Session.Template != "" {
		t.Errorf("session template = %q, want empty", got.Session.Template)
	}
	if len(drv.templates) != 1 || drv.templates[0] != "" {
		t.Errorf("driver saw templates %v, want one empty", drv.templates)
	}
}

// The list of choices is the first environment that can answer. A kind whose
// environments cannot be read yields nothing rather than an error, because a lab
// can still be asked for — it just gets the environment's default.
// A kind's templates are read once and kept, because /config is polled every
// few seconds and reading them means a request to the environment. A read that
// fails is not kept, so an environment coming up starts offering its templates
// without waiting for a restart.
func TestChoicesForReadsOnceAndKeepsWhatItRead(t *testing.T) {
	drv := &fakeDriver{ready: true, choices: []driver.Choice{{ID: "e2b", Title: "E2B"}}}
	m, _ := testManager(t, drv, testConfig())

	got := m.ChoicesFor(context.Background(), model.KindApplab)
	if len(got) != 1 || got[0].ID != "e2b" {
		t.Fatalf("ChoicesFor = %+v, want the driver's one choice", got)
	}
	// The environment goes away, but the list it gave is still the list.
	drv.choicesErr = errors.New("unreachable")
	if got := m.ChoicesFor(context.Background(), model.KindApplab); len(got) != 1 {
		t.Errorf("a cached list should survive its environment, got %+v", got)
	}
}

func TestChoicesForDoesNotCacheAReadThatFailed(t *testing.T) {
	drv := &fakeDriver{ready: true, choicesErr: errors.New("unreachable")}
	m, _ := testManager(t, drv, testConfig())

	if got := m.ChoicesFor(context.Background(), model.KindApplab); len(got) != 0 {
		t.Errorf("an unreachable environment should yield no choices, got %+v", got)
	}
	// It comes back, and the next read finds the templates rather than a cached
	// absence that would hide the picker for as long as the process lives.
	drv.choicesErr = nil
	drv.choices = []driver.Choice{{ID: "e2b", Title: "E2B"}}
	if got := m.ChoicesFor(context.Background(), model.KindApplab); len(got) != 1 {
		t.Errorf("ChoicesFor = %+v, want the list once the environment answers", got)
	}
}
