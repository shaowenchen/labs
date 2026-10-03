package config

import (
	"strings"
	"testing"
	"time"
)

// setEnv sets the variables a valid configuration needs, so a test only has to
// override the one it is about.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		"LABS_GITHUB_TOKEN": "token",
		"LABS_REPOS":        "o/applab,o/sandboxlab",
		"LABS_ENVIRONMENTS": `[{"id":"applab-1","kind":"applab","repo":"o/applab","workflow":"debugger.yml","ref":"main","domain":"a.example.com","base_path":"/applab","slots":["lab-01"],"capacity":1}]`,
		"LABS_KEY_APPLAB_1": "admin-key",
	}
	for k, v := range override(base, overrides) {
		t.Setenv(k, v)
	}
}

func override(base, over map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

func TestLoadResolvesTheDefaults(t *testing.T) {
	setEnv(t, nil)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SessionTTL != 2*time.Hour {
		t.Errorf("SessionTTL = %s, want 2h", cfg.SessionTTL)
	}
	if !cfg.KeepWarm {
		t.Error("KeepWarm should default on")
	}
	if len(cfg.Envs) != 1 || cfg.Envs[0].APIKey != "admin-key" {
		t.Fatalf("the environment or its key did not load: %+v", cfg.Envs)
	}
	if cfg.SessionCeiling() != 1 {
		t.Errorf("SessionCeiling = %d, want the environment's capacity", cfg.SessionCeiling())
	}
}

// The point of the loader's design: an empty environment is not an error. The
// service starts and says what is missing.
func TestLoadWithNothingSetStillReturnsAConfig(t *testing.T) {
	// Clear anything the ambient environment might have.
	for _, k := range []string{"LABS_GITHUB_TOKEN", "LABS_REPOS", "LABS_ENVIRONMENTS", "LABS_KEY_APPLAB_1", "LABS_LISTEN"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned an error for an empty environment: %v", err)
	}
	if cfg.Usable() {
		t.Fatal("a configuration with nothing set reported itself usable")
	}
	for _, want := range []string{"LABS_GITHUB_TOKEN", "LABS_REPOS", "LABS_ENVIRONMENTS"} {
		if !hasProblem(cfg.Problems, want) {
			t.Errorf("problems do not mention %s: %v", want, cfg.Problems)
		}
	}
	if cfg.Listen == "" {
		t.Error("Listen must still resolve so the server can bind")
	}
}

// A variable set to an unreadable value is the one case that is still fatal: it
// is a typo in a variable this service owns, and continuing would silently
// substitute the default for the value the operator meant.
func TestLoadFailsOnAnUnreadableDuration(t *testing.T) {
	setEnv(t, map[string]string{"LABS_SESSION_TTL": "two hours"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "LABS_SESSION_TTL") {
		t.Fatalf("Load = %v, want an error naming LABS_SESSION_TTL", err)
	}
}

func TestMissingTokenIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_GITHUB_TOKEN": ""})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "LABS_GITHUB_TOKEN") {
		t.Fatalf("want a problem naming LABS_GITHUB_TOKEN, got %v", cfg.Problems)
	}
}

func TestMissingKeyPerEnvironmentIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_KEY_APPLAB_1": ""})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "LABS_KEY_APPLAB_1") {
		t.Fatalf("want a problem naming the missing key variable, got %v", cfg.Problems)
	}
}

func TestEnvironmentOutsideReposIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_REPOS": "o/other"})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "LABS_REPOS") {
		t.Fatalf("want a problem about the repo not being allowed, got %v", cfg.Problems)
	}
}

func TestInvalidSessionHoursIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_DISPATCH_SESSION_HOURS": "3"})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "LABS_DISPATCH_SESSION_HOURS") {
		t.Fatalf("want a problem about the session-hours choice, got %v", cfg.Problems)
	}
}

func TestApplabWithoutSlotsIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_ENVIRONMENTS": `[{"id":"applab-1","kind":"applab","repo":"o/applab","workflow":"debugger.yml","ref":"main","domain":"a.example.com","capacity":1}]`,
	})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "slots") {
		t.Fatalf("want a problem about missing slots, got %v", cfg.Problems)
	}
}

func TestBadDomainIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_ENVIRONMENTS": `[{"id":"applab-1","kind":"applab","repo":"o/applab","workflow":"debugger.yml","ref":"main","domain":"https://a.example.com","base_path":"/applab","slots":["lab-01"],"capacity":1}]`,
	})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "domain") {
		t.Fatalf("want a problem about the domain shape, got %v", cfg.Problems)
	}
}

func TestMalformedEnvironmentsJSONIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_ENVIRONMENTS": `{not an array}`})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "LABS_ENVIRONMENTS") {
		t.Fatalf("want a problem about the JSON, got %v", cfg.Problems)
	}
}

// Every problem is reported at once, so one restart shows everything to fix
// rather than one thing per attempt.
func TestAllProblemsAreReportedTogether(t *testing.T) {
	for _, k := range []string{"LABS_GITHUB_TOKEN", "LABS_REPOS", "LABS_ENVIRONMENTS"} {
		t.Setenv(k, "")
	}
	cfg := mustLoad(t)
	if len(cfg.Problems) < 3 {
		t.Fatalf("want the missing variables reported together, got %v", cfg.Problems)
	}
}

func mustLoad(t *testing.T) Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func hasProblem(problems []string, substr string) bool {
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

func TestSessionCeilingPrefersTheConfiguredValue(t *testing.T) {
	setEnv(t, map[string]string{"LABS_MAX_SESSIONS": "7"})
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionCeiling() != 7 {
		t.Errorf("SessionCeiling = %d, want the configured 7", cfg.SessionCeiling())
	}
}

func TestKeyEnvSuffix(t *testing.T) {
	cases := map[string]string{"applab-1": "APPLAB_1", "sandbox-2": "SANDBOX_2", "A.b-c": "A_B_C"}
	for in, want := range cases {
		if got := keyEnvSuffix(in); got != want {
			t.Errorf("keyEnvSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListenResolution(t *testing.T) {
	// LABS_LISTEN wins; PORT is honoured when it is not set; otherwise :8080.
	cases := []struct {
		name string
		labs string
		port string
		want string
	}{
		{"neither set", "", "", ":8080"},
		{"PORT only", "", "9000", ":9000"},
		{"LABS_LISTEN only", ":7777", "", ":7777"},
		{"both set, LABS_LISTEN wins", ":7777", "9000", ":7777"},
		{"blank PORT falls through", "", "  ", ":8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.labs != "" {
				t.Setenv("LABS_LISTEN", tc.labs)
			}
			if tc.port != "" {
				t.Setenv("PORT", tc.port)
			}
			if got := resolveListen(tc.labs); got != tc.want {
				t.Errorf("resolveListen(%q) = %q, want %q", tc.labs, got, tc.want)
			}
		})
	}
}
