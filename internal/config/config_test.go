package config

import (
	"strings"
	"testing"
	"time"
)

// setEnv sets the variables a valid single-applab-repo configuration needs, so
// a test only has to override the one it is about.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	base := map[string]string{
		"LABS_GITHUB_TOKEN":  "token",
		"LABS_REPOS":         "o/applab",
		"LABS_DOMAIN_SUFFIX": "example.com",
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

// One repository is one environment, and everything a repository implies is
// derived from its name.
func TestRepositoryBecomesAnEnvironment(t *testing.T) {
	setEnv(t, nil)
	cfg := mustLoad(t)

	if !cfg.Usable() {
		t.Fatalf("a minimal configuration should be usable, problems: %v", cfg.Problems)
	}
	if len(cfg.Envs) != 1 {
		t.Fatalf("want one environment from one repository, got %d", len(cfg.Envs))
	}
	e := cfg.Envs[0]
	if e.ID != "APPLAB" {
		t.Errorf("id = %q, want APPLAB", e.ID)
	}
	if e.Kind != "applab" {
		t.Errorf("kind = %q, want applab", e.Kind)
	}
	if e.Repo != "o/applab" {
		t.Errorf("repo = %q", e.Repo)
	}
	if e.Workflow != "debugger.yml" {
		t.Errorf("workflow = %q, want debugger.yml", e.Workflow)
	}
	if e.BasePath != "/applab" {
		t.Errorf("base_path = %q, want /applab", e.BasePath)
	}
	if e.Domain != "applab.example.com" {
		t.Errorf("domain = %q, want applab.example.com (derived from the suffix)", e.Domain)
	}
	// No key was configured, so labs falls back to the fixed default — the same
	// value on every run and across every restart, which is what lets it reach
	// an environment it did not itself just dispatch.
	if e.APIKey != defaultAPIKey {
		t.Errorf("api key = %q, want the default %q", e.APIKey, defaultAPIKey)
	}
	// The key is handed over at dispatch, so the environment comes up holding it.
	if got := e.DispatchInputs("4")["api_key"]; got != defaultAPIKey {
		t.Errorf("dispatch api_key = %q, want the default", got)
	}
	if e.Capacity != defaultEnvSlots || len(e.Slots) != defaultEnvSlots {
		t.Errorf("capacity = %d, slots = %d, want %d of each", e.Capacity, len(e.Slots), defaultEnvSlots)
	}
	if e.Capacity != len(e.Slots) {
		t.Errorf("capacity %d and slots %d must match for applab", e.Capacity, len(e.Slots))
	}
}

func TestSandboxlabRepositoryIsRecognised(t *testing.T) {
	setEnv(t, map[string]string{"LABS_REPOS": "o/sandboxlab"})
	cfg := mustLoad(t)
	if len(cfg.Envs) != 1 {
		t.Fatalf("want one environment, got %d", len(cfg.Envs))
	}
	e := cfg.Envs[0]
	if e.Kind != "sandboxlab" {
		t.Errorf("kind = %q, want sandboxlab", e.Kind)
	}
	if e.ID != "SANDBOXLAB" {
		t.Errorf("id = %q, want SANDBOXLAB", e.ID)
	}
	if e.BasePath != "/sandbox" {
		t.Errorf("base_path = %q, want /sandbox", e.BasePath)
	}
	if e.Workflow != "debugger.yml" {
		t.Errorf("workflow = %q, want debugger.yml", e.Workflow)
	}
	if e.Domain != "sandboxlab.example.com" {
		t.Errorf("domain = %q, want sandboxlab.example.com", e.Domain)
	}
}

func TestSeveralRepositoriesBecomeSeveralEnvironments(t *testing.T) {
	setEnv(t, map[string]string{"LABS_REPOS": "o/applab,o/sandboxlab"})
	cfg := mustLoad(t)
	if len(cfg.Envs) != 2 {
		t.Fatalf("want two environments, got %d", len(cfg.Envs))
	}
	if cfg.TotalCapacity() != 2*defaultEnvSlots {
		t.Errorf("TotalCapacity = %d, want %d", cfg.TotalCapacity(), 2*defaultEnvSlots)
	}
}

// A per-environment domain overrides the suffix, for one that does not follow
// the naming.
func TestPerEnvironmentDomainOverridesTheSuffix(t *testing.T) {
	setEnv(t, map[string]string{"LABS_DOMAIN_APPLAB": "custom.example.org"})
	if got := mustLoad(t).Envs[0].Domain; got != "custom.example.org" {
		t.Errorf("domain = %q, want the per-environment value", got)
	}
}

// A configured key is used as-is, and is sent in the dispatch like any other:
// there is one way the key reaches an environment, not two.
func TestConfiguredKeyIsUsedAsIs(t *testing.T) {
	setEnv(t, map[string]string{"LABS_KEY_APPLAB": "my-own-key"})
	e := mustLoad(t).Envs[0]
	if e.APIKey != "my-own-key" {
		t.Errorf("api key = %q, want the configured value", e.APIKey)
	}
	if got := e.DispatchInputs("4")["api_key"]; got != "my-own-key" {
		t.Errorf("dispatch api_key = %q, want the configured key", got)
	}
}

// A generated key is carried in the dispatch, so the environment comes up with
// the key labs holds.
// The default key is carried in the dispatch for applab, so the environment
// comes up holding the key labs will mint per-app keys with.
func TestDefaultKeyIsCarriedInTheApplabDispatch(t *testing.T) {
	setEnv(t, nil)
	e := mustLoad(t).Envs[0]
	if e.APIKey != defaultAPIKey {
		t.Fatalf("api key = %q, want the default", e.APIKey)
	}
	if got := e.DispatchInputs("4")["api_key"]; got != defaultAPIKey {
		t.Errorf("dispatch api_key = %q, want the default", got)
	}
}

func TestDispatchInputsCarryTheDomain(t *testing.T) {
	setEnv(t, nil)
	e := mustLoad(t).Envs[0]
	inputs := e.DispatchInputs("4")
	if inputs["domain"] != "applab.example.com" {
		t.Errorf("dispatch domain = %q", inputs["domain"])
	}
	if inputs["session_hours"] != "4" {
		t.Errorf("dispatch session_hours = %q", inputs["session_hours"])
	}
	if inputs["tunnel"] != "cloudflare" {
		t.Errorf("dispatch tunnel = %q, want cloudflare", inputs["tunnel"])
	}
}

func TestLabSlots(t *testing.T) {
	cases := map[int][]string{
		0: nil,
		1: {"lab-01"},
		4: {"lab-01", "lab-02", "lab-03", "lab-04"},
	}
	for n, want := range cases {
		got := slotNames(n)
		if len(got) != len(want) {
			t.Fatalf("slotNames(%d) = %v, want %v", n, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("slotNames(%d)[%d] = %q, want %q", n, i, got[i], want[i])
			}
		}
	}
}

func TestEnvID(t *testing.T) {
	cases := map[string]string{"applab": "APPLAB", "sandboxlab": "SANDBOXLAB", "my-app": "MY_APP", "A.b": "A_B"}
	for in, want := range cases {
		if got := envID(in); got != want {
			t.Errorf("envID(%q) = %q, want %q", in, got, want)
		}
	}
}

// The point of the loader's design: an empty environment is not an error. The
// service starts and says what is missing.
func TestLoadWithNothingSetStillReturnsAConfig(t *testing.T) {
	for _, k := range []string{"LABS_GITHUB_TOKEN", "LABS_REPOS", "LABS_LISTEN"} {
		t.Setenv(k, "")
	}
	cfg := mustLoad(t)
	if cfg.Usable() {
		t.Fatal("a configuration with nothing set reported itself usable")
	}
	for _, want := range []string{"LABS_GITHUB_TOKEN", "LABS_REPOS"} {
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

func TestMissingRepoIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_REPOS": ""})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "LABS_REPOS") {
		t.Fatalf("want a problem naming LABS_REPOS, got %v", cfg.Problems)
	}
}

// A key left out is not a problem: labs falls back to the fixed default and
// hands it to the environment in the dispatch. That is what lets a deployment
// run on a token and a repo list alone.
func TestMissingKeyFallsBackToTheDefaultNotAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_KEY_APPLAB": ""})
	cfg := mustLoad(t)
	if !cfg.Usable() {
		t.Fatalf("a configuration without a key should still be usable, problems: %v", cfg.Problems)
	}
	if cfg.Envs[0].APIKey != defaultAPIKey {
		t.Fatalf("want the default key, got %+v", cfg.Envs[0])
	}
}

// With no domain configured, each kind falls back to the hostname its own
// workflow declares as a choice. That is what lets a deployment be a token and a
// repository list.
func TestDomainFallsBackToTheProjectsDefault(t *testing.T) {
	setEnv(t, map[string]string{"LABS_DOMAIN_SUFFIX": ""})
	cfg := mustLoad(t)
	if !cfg.Usable() {
		t.Fatalf("a configuration without a domain should be usable, problems: %v", cfg.Problems)
	}
	if cfg.Envs[0].Domain != "applab-2.chenshaowen.com" {
		t.Errorf("domain = %q, want the applab default", cfg.Envs[0].Domain)
	}
}

// A domain that is set to something unusable is still a problem: it is a typo,
// and silently ignoring it would hide it.
func TestBadDomainIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_DOMAIN_APPLAB": "https://a.example.com"})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "domain") {
		t.Fatalf("want a problem about the domain shape, got %v", cfg.Problems)
	}
}

func TestInvalidSessionHoursIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_DISPATCH_SESSION_HOURS": "3"})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "LABS_DISPATCH_SESSION_HOURS") {
		t.Fatalf("want a problem about the session-hours choice, got %v", cfg.Problems)
	}
}

func TestZeroSlotsIsAProblem(t *testing.T) {
	setEnv(t, map[string]string{"LABS_ENV_SLOTS": "0"})
	cfg := mustLoad(t)
	if cfg.Usable() || !hasProblem(cfg.Problems, "LABS_ENV_SLOTS") {
		t.Fatalf("want a problem about the slot count, got %v", cfg.Problems)
	}
}

// Every problem is reported at once, so one restart shows everything to fix
// rather than one thing per attempt.
func TestAllProblemsAreReportedTogether(t *testing.T) {
	for _, k := range []string{"LABS_GITHUB_TOKEN", "LABS_REPOS"} {
		t.Setenv(k, "")
	}
	cfg := mustLoad(t)
	if len(cfg.Problems) < 2 {
		t.Fatalf("want the missing variables reported together, got %v", cfg.Problems)
	}
}

func TestEnvSlotsOverride(t *testing.T) {
	setEnv(t, map[string]string{"LABS_ENV_SLOTS": "2"})
	cfg := mustLoad(t)
	if cfg.Envs[0].Capacity != 2 || len(cfg.Envs[0].Slots) != 2 {
		t.Fatalf("LABS_ENV_SLOTS=2 gave capacity %d and %d slots", cfg.Envs[0].Capacity, len(cfg.Envs[0].Slots))
	}
}

func TestSessionCeilingPrefersTheConfiguredValue(t *testing.T) {
	setEnv(t, map[string]string{"LABS_MAX_SESSIONS": "7"})
	if got := mustLoad(t).SessionCeiling(); got != 7 {
		t.Errorf("SessionCeiling = %d, want the configured 7", got)
	}
}

func TestListenResolution(t *testing.T) {
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

func TestSessionTTLDefault(t *testing.T) {
	setEnv(t, nil)
	if got := mustLoad(t).SessionTTL; got != 2*time.Hour {
		t.Errorf("SessionTTL = %s, want 2h", got)
	}
}

// A sandboxlab repository gets its kind, path, and template, and falls back to
// the same fixed default key as applab — one key for every kind, sent in the
// dispatch, so a sandboxlab environment comes up holding the key labs calls it
// with.
func TestSandboxlabUsesTheDefaultKey(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_REPOS":               "o/sandboxlab",
		"LABS_DOMAIN_SANDBOXLAB":   "sandboxlab-1.example.com",
		"LABS_TEMPLATE_SANDBOXLAB": "all-in-one",
	})
	cfg := mustLoad(t)
	if !cfg.Usable() {
		t.Fatalf("a sandboxlab environment with no key should be usable, problems: %v", cfg.Problems)
	}
	e := cfg.Envs[0]
	if e.APIKey != defaultAPIKey {
		t.Fatalf("api key = %q, want the default %q", e.APIKey, defaultAPIKey)
	}
	if got := e.DispatchInputs("4")["api_key"]; got != defaultAPIKey {
		t.Errorf("sandboxlab dispatch api_key = %q, want the default", got)
	}
}

func TestSandboxlabWithAKeyIsUsable(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_REPOS":               "o/sandboxlab",
		"LABS_DOMAIN_SANDBOXLAB":   "sandboxlab-1.example.com",
		"LABS_KEY_SANDBOXLAB":      "sb-key",
		"LABS_TEMPLATE_SANDBOXLAB": "all-in-one",
	})
	cfg := mustLoad(t)
	if !cfg.Usable() {
		t.Fatalf("a fully configured sandboxlab environment should be usable: %v", cfg.Problems)
	}
	e := cfg.Envs[0]
	if e.Kind != "sandboxlab" || e.BasePath != "/sandbox" || e.Workflow != "debugger.yml" {
		t.Errorf("sandboxlab env = %+v", e)
	}
	if e.Template != "all-in-one" {
		t.Errorf("template = %q, want all-in-one", e.Template)
	}
	if got := e.DispatchInputs("4")["api_key"]; got != "sb-key" {
		t.Errorf("dispatch api_key = %q, want the configured key", got)
	}
}

// The sandboxlab dispatch carries the domain and the key, exactly as applab's
// does: one key reaches every kind the same way.
func TestSandboxlabDispatchCarriesTheKeyAndDomain(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_REPOS":             "o/sandboxlab",
		"LABS_DOMAIN_SANDBOXLAB": "sandboxlab-1.example.com",
	})
	inputs := mustLoad(t).Envs[0].DispatchInputs("4")
	if inputs["api_key"] != defaultAPIKey {
		t.Errorf("sandboxlab dispatch api_key = %q, want the default", inputs["api_key"])
	}
	if inputs["domain"] != "sandboxlab-1.example.com" {
		t.Errorf("domain = %v, want the configured one", inputs["domain"])
	}
}

// The default domain is sent in the dispatch, because the workflow's domain
// input is a choice and the default is one it accepts.
func TestDefaultDomainIsSentInTheDispatch(t *testing.T) {
	setEnv(t, map[string]string{"LABS_DOMAIN_SUFFIX": ""})
	inputs := mustLoad(t).Envs[0].DispatchInputs("4")
	if inputs["domain"] != "applab-2.chenshaowen.com" {
		t.Errorf("domain = %v, want the applab default", inputs["domain"])
	}
	if inputs["session_hours"] != "4" || inputs["tunnel"] != "cloudflare" {
		t.Errorf("inputs = %v", inputs)
	}
}

// A domain configured to empty is sent as no domain input, for a workflow that
// would take a default of its own.
func TestEmptyDomainSendsNoDomainInput(t *testing.T) {
	setEnv(t, map[string]string{"LABS_DOMAIN_SUFFIX": ""})
	e := mustLoad(t).Envs[0]
	e.Domain = "" // as a deployment with a blank override would have it
	if _, ok := e.DispatchInputs("4")["domain"]; ok {
		t.Errorf("an empty domain should not be sent: %v", e.DispatchInputs("4"))
	}
}

// LABS_ACTION_API_KEY pins one key for every action, which is how a deployment
// gives a single fixed value rather than relying on the built-in default. It
// reaches every kind the same way — sent in the dispatch — so one key is all
// that is needed to call any of them.
func TestOneActionKeyForEveryEnvironment(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_REPOS":          "o/applab,o/sandboxlab",
		"LABS_ACTION_API_KEY": "one-action-key",
	})
	cfg := mustLoad(t)
	if len(cfg.Envs) != 2 {
		t.Fatalf("want two environments, got %d", len(cfg.Envs))
	}
	for _, e := range cfg.Envs {
		if e.APIKey != "one-action-key" {
			t.Errorf("%s key = %q, want the configured one", e.ID, e.APIKey)
		}
		if got := e.DispatchInputs("4")["api_key"]; got != "one-action-key" {
			t.Errorf("%s dispatch api_key = %q, want the configured key", e.ID, got)
		}
	}
}

// A per-environment key overrides the shared one, for the one environment that
// does not use it.
func TestPerEnvironmentKeyOverridesTheSharedOne(t *testing.T) {
	setEnv(t, map[string]string{
		"LABS_REPOS":          "o/applab,o/sandboxlab",
		"LABS_ACTION_API_KEY": "one-action-key",
		"LABS_KEY_SANDBOXLAB": "its-own-key",
	})
	cfg := mustLoad(t)
	byID := map[string]string{}
	for _, e := range cfg.Envs {
		byID[e.ID] = e.APIKey
	}
	if byID["APPLAB"] != "one-action-key" {
		t.Errorf("APPLAB key = %q, want the shared one", byID["APPLAB"])
	}
	if byID["SANDBOXLAB"] != "its-own-key" {
		t.Errorf("SANDBOXLAB key = %q, want its own", byID["SANDBOXLAB"])
	}
}
