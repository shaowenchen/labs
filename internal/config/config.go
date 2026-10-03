// Package config resolves the service's settings from the environment.
//
// Everything is an environment variable, set by the compose file or the chart.
// That keeps the binary free of a config file format and of flags that would
// have to be kept in step with the deployment, and it means the deployment's
// settings are readable from the pod spec — which is where someone debugging a
// deployment looks first.
//
// The one thing that is not a scalar is the set of environments, which is
// necessarily a list. It is JSON in LABS_ENVIRONMENTS, with each environment's
// secret in its own LABS_KEY_<ID> variable, so the JSON can be logged at
// startup without ever printing a key.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shaowenchen/labs/internal/model"
)

// Config is the resolved configuration.
type Config struct {
	// Listen is the address the HTTP server binds, in Go's ":port" form.
	Listen string

	// SessionTTL is how long a delivered lab lasts, which is the product's
	// promised validity.
	SessionTTL time.Duration

	// ReapInterval is how often expired sessions are collected.
	ReapInterval time.Duration

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// GitHubToken authenticates the dispatcher. It needs Actions: write on
	// every repository in Repos.
	GitHubToken string

	// GitHubAPI is the REST base. Overridable so tests and GitHub Enterprise
	// can point it elsewhere.
	GitHubAPI string

	// DispatchRef is the git ref workflows are dispatched on.
	DispatchRef string

	// DispatchSessionHours is the `session_hours` input passed to a dispatched
	// workflow. The workflows declare it as a choice, so it must be one of
	// 1, 2, 4 or unlimited.
	DispatchSessionHours string

	// KeepWarm turns on the keeper that dispatches successor runs so an
	// environment is always up. Off means an environment is started on demand,
	// which costs a cold boot on the first request.
	KeepWarm bool

	// KeepWarmInterval is how often the keeper looks at each environment.
	KeepWarmInterval time.Duration

	// RedispatchMargin is how long before a run's expected end the keeper
	// queues its successor.
	RedispatchMargin time.Duration

	// MaxSessions caps concurrent sessions across all environments. Zero means
	// derive it from the environments' own capacities.
	MaxSessions int

	// MaxSessionsPerIP caps concurrent sessions from one address.
	MaxSessionsPerIP int

	// RateLimitCount is how many session requests one address may make in
	// RateLimitWindow.
	RateLimitCount int

	// RateLimitWindow is the window RateLimitCount applies over.
	RateLimitWindow time.Duration

	// TrustedProxy says whether X-Forwarded-For may be believed for the client
	// address. It must only be set when the service sits behind a proxy that
	// sets the header and the service's own port is not reachable directly, or
	// every limiter becomes bypassable with a header.
	TrustedProxy bool

	// Repos is the allow-list of repositories workflows may be dispatched in,
	// as "owner/repo". An environment naming a repository outside it is refused
	// at load.
	Repos []string

	// Envs are the environments, each with its key resolved.
	Envs []model.Env
}

// Load reads the configuration from the environment and validates it.
func Load() (Config, error) {
	var cfg Config
	var err error

	setString(&cfg.Listen, "LABS_LISTEN", ":8080")
	setString(&cfg.LogLevel, "LABS_LOG_LEVEL", "info")
	setString(&cfg.GitHubToken, "LABS_GITHUB_TOKEN", "")
	setString(&cfg.GitHubAPI, "LABS_GITHUB_API", "https://api.github.com")
	setString(&cfg.DispatchRef, "LABS_DISPATCH_REF", "main")
	setString(&cfg.DispatchSessionHours, "LABS_DISPATCH_SESSION_HOURS", "4")

	cfg.Repos = splitList(os.Getenv("LABS_REPOS"))

	if cfg.SessionTTL, err = durationEnv("LABS_SESSION_TTL", 2*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.ReapInterval, err = durationEnv("LABS_REAP_INTERVAL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.KeepWarmInterval, err = durationEnv("LABS_KEEPWARM_INTERVAL", time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.RedispatchMargin, err = durationEnv("LABS_REDISPATCH_MARGIN", 45*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitWindow, err = durationEnv("LABS_RATE_LIMIT_WINDOW", time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.MaxSessions, err = intEnv("LABS_MAX_SESSIONS", 0); err != nil {
		return Config{}, err
	}
	if cfg.MaxSessionsPerIP, err = intEnv("LABS_MAX_SESSIONS_PER_IP", 1); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitCount, err = intEnv("LABS_RATE_LIMIT_COUNT", 5); err != nil {
		return Config{}, err
	}
	if cfg.KeepWarm, err = boolEnv("LABS_KEEPWARM", true); err != nil {
		return Config{}, err
	}
	if cfg.TrustedProxy, err = boolEnv("LABS_TRUSTED_PROXY", false); err != nil {
		return Config{}, err
	}

	if cfg.Envs, err = parseEnvs(os.Getenv("LABS_ENVIRONMENTS")); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects a configuration the service could not run under.
//
// Every message names the variable at fault. A configuration error that only
// says what is wrong, not where, costs the reader a search through the deploy
// manifest for something the process already knew.
func (c Config) Validate() error {
	switch {
	case c.Listen == "":
		return fmt.Errorf("LABS_LISTEN is empty")
	case c.SessionTTL <= 0:
		return fmt.Errorf("LABS_SESSION_TTL must be positive, got %s", c.SessionTTL)
	case c.GitHubToken == "":
		return fmt.Errorf("LABS_GITHUB_TOKEN is required: the service dispatches workflows, which needs a token with Actions: write")
	case len(c.Repos) == 0:
		return fmt.Errorf("LABS_REPOS is required: a comma-separated list of owner/repo the service may dispatch in")
	case len(c.Envs) == 0:
		return fmt.Errorf("LABS_ENVIRONMENTS is required: a JSON array describing at least one environment")
	}

	for _, r := range c.Repos {
		if !strings.Contains(r, "/") {
			return fmt.Errorf("LABS_REPOS entry %q is not owner/repo", r)
		}
	}

	if !validSessionHours(c.DispatchSessionHours) {
		return fmt.Errorf("LABS_DISPATCH_SESSION_HOURS is %q, but the workflow declares it as a choice of 1, 2, 4 or unlimited", c.DispatchSessionHours)
	}

	seen := make(map[string]bool, len(c.Envs))
	for _, e := range c.Envs {
		if err := c.validateEnv(e); err != nil {
			return err
		}
		if seen[e.ID] {
			return fmt.Errorf("LABS_ENVIRONMENTS names %q twice", e.ID)
		}
		seen[e.ID] = true
	}
	return nil
}

func (c Config) validateEnv(e model.Env) error {
	switch {
	case e.ID == "":
		return fmt.Errorf("LABS_ENVIRONMENTS has an environment with no id")
	case !e.Kind.Known():
		return fmt.Errorf("LABS_ENVIRONMENTS entry %q has kind %q, which is not applab or sandboxlab", e.ID, e.Kind)
	case e.Repo == "":
		return fmt.Errorf("LABS_ENVIRONMENTS entry %q names no repo", e.ID)
	case !contains(c.Repos, e.Repo):
		return fmt.Errorf("LABS_ENVIRONMENTS entry %q dispatches in %q, which is not in LABS_REPOS", e.ID, e.Repo)
	case e.Workflow == "":
		return fmt.Errorf("LABS_ENVIRONMENTS entry %q names no workflow", e.ID)
	case e.Ref == "":
		return fmt.Errorf("LABS_ENVIRONMENTS entry %q names no ref", e.ID)
	case !validHost(e.Domain):
		return fmt.Errorf("LABS_ENVIRONMENTS entry %q has domain %q, which is not a bare hostname", e.ID, e.Domain)
	case !validBasePath(e.BasePath):
		return fmt.Errorf("LABS_ENVIRONMENTS entry %q has base_path %q, which must be empty or start with / and not end with one", e.ID, e.BasePath)
	case e.APIKey == "":
		return fmt.Errorf("no key for environment %q: set LABS_KEY_%s to the key the environment is configured with", e.ID, keyEnvSuffix(e.ID))
	case e.Capacity <= 0:
		return fmt.Errorf("LABS_ENVIRONMENTS entry %q has capacity %d, which must be positive", e.ID, e.Capacity)
	}

	switch e.Kind {
	case model.KindApplab:
		if len(e.Slots) == 0 {
			return fmt.Errorf("LABS_ENVIRONMENTS entry %q is an applab environment with no slots: it needs one app id per concurrent session", e.ID)
		}
		if e.Capacity != len(e.Slots) {
			return fmt.Errorf("LABS_ENVIRONMENTS entry %q has capacity %d but %d slots; for applab they are the same number", e.ID, e.Capacity, len(e.Slots))
		}
	case model.KindSandboxlab:
		if e.Template == "" {
			return fmt.Errorf("LABS_ENVIRONMENTS entry %q is a sandboxlab environment with no template", e.ID)
		}
	}
	return nil
}

// TotalCapacity is how many concurrent sessions the environments can serve.
func (c Config) TotalCapacity() int {
	n := 0
	for _, e := range c.Envs {
		n += e.Capacity
	}
	return n
}

// SessionCeiling is the effective global cap on concurrent sessions: the
// configured value when set, otherwise everything the environments can hold.
func (c Config) SessionCeiling() int {
	if c.MaxSessions > 0 {
		return c.MaxSessions
	}
	return c.TotalCapacity()
}

// EnvByID returns the environment with the given id.
func (c Config) EnvByID(id string) (model.Env, bool) {
	for _, e := range c.Envs {
		if e.ID == id {
			return e, true
		}
	}
	return model.Env{}, false
}

// parseEnvs reads LABS_ENVIRONMENTS and attaches each environment's key from
// its own LABS_KEY_<ID> variable.
//
// The split is the point: the JSON holds only what is safe to log, and the
// secret is read from a variable named for the environment so a key can be
// rotated without editing the JSON.
func parseEnvs(raw string) ([]model.Env, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var envs []model.Env
	if err := json.Unmarshal([]byte(raw), &envs); err != nil {
		return nil, fmt.Errorf("LABS_ENVIRONMENTS is not valid JSON: %w", err)
	}
	for i := range envs {
		envs[i].APIKey = strings.TrimSpace(os.Getenv("LABS_KEY_" + keyEnvSuffix(envs[i].ID)))
	}
	return envs, nil
}

// keyEnvSuffix turns an environment id into the tail of its key variable:
// "applab-1" becomes "APPLAB_1", so the variable is LABS_KEY_APPLAB_1.
func keyEnvSuffix(id string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(id) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func validSessionHours(v string) bool {
	switch v {
	case "1", "2", "4", "unlimited":
		return true
	default:
		return false
	}
}

// validHost reports whether v is a bare hostname: no scheme, no path, and a
// dot somewhere. It is a shape check, not a resolver — a wrong-but-well-formed
// domain fails at the first poll, which is the right place to find it.
func validHost(v string) bool {
	if v == "" || strings.ContainsAny(v, " /:") {
		return false
	}
	if strings.HasPrefix(v, ".") || strings.HasSuffix(v, ".") || strings.HasSuffix(v, "-") {
		return false
	}
	return strings.Contains(v, ".")
}

func validBasePath(v string) bool {
	if v == "" {
		return true
	}
	return strings.HasPrefix(v, "/") && !strings.HasSuffix(v, "/")
}

// ── small env readers ───────────────────────────────────────────────────────

func setString(dst *string, key, def string) {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		*dst = strings.TrimSpace(v)
		return
	}
	*dst = def
}

func durationEnv(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func intEnv(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func boolEnv(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
