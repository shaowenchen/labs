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

func TestLoadRequiresAToken(t *testing.T) {
	setEnv(t, map[string]string{"LABS_GITHUB_TOKEN": ""})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "LABS_GITHUB_TOKEN") {
		t.Fatalf("Load = %v, want an error naming LABS_GITHUB_TOKEN", err)
	}
}

func TestLoadRequiresAKeyPerEnvironment(t *testing.T) {
	setEnv(t, map[string]string{"LABS_KEY_APPLAB_1": ""})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "LABS_KEY_APPLAB_1") {
		t.Fatalf("Load = %v, want an error naming the missing key variable", err)
	}
}

func TestEnvironmentOutsideReposIsRefused(t *testing.T) {
	setEnv(t, map[string]string{"LABS_REPOS": "o/other"})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "LABS_REPOS") {
		t.Fatalf("Load = %v, want an error about the repo not being allowed", err)
	}
}

func TestInvalidSessionHoursIsRefused(t *testing.T) {
	setEnv(t, map[string]string{"LABS_DISPATCH_SESSION_HOURS": "3"})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "LABS_DISPATCH_SESSION_HOURS") {
		t.Fatalf("Load = %v, want an error about the session-hours choice", err)
	}
}

func TestApplabWithoutSlotsIsRefused(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_ENVIRONMENTS": `[{"id":"applab-1","kind":"applab","repo":"o/applab","workflow":"debugger.yml","ref":"main","domain":"a.example.com","capacity":1}]`,
	})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "slots") {
		t.Fatalf("Load = %v, want an error about missing slots", err)
	}
}

func TestBadDomainIsRefused(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_ENVIRONMENTS": `[{"id":"applab-1","kind":"applab","repo":"o/applab","workflow":"debugger.yml","ref":"main","domain":"https://a.example.com","base_path":"/applab","slots":["lab-01"],"capacity":1}]`,
	})
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "domain") {
		t.Fatalf("Load = %v, want an error about the domain shape", err)
	}
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
