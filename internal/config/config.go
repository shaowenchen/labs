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

	// Problems are the configuration mistakes found while loading: a variable
	// that could not be read, a required one that is missing, an environment
	// that is not shaped right. They do not stop the service. It starts, serves
	// /healthz, and reports them through its log, /readyz and /api/v1/config.
	Problems []string
}

// Load reads the configuration from the environment.
//
// It returns an error only when the environment cannot be read at all — a
// malformed duration or number in a variable this service owns. Anything else
// that is wrong, including everything a working deployment needs and does not
// have, comes back in Problems instead.
//
// The reason is the failure it prevents. A process that exits on incomplete
// configuration is indistinguishable, from the outside, from one that crashed,
// and on a platform that starts it per request it is a 500 on every request.
// Starting anyway means /healthz answers, /readyz and /api/v1/config say exactly
// what is missing, and whoever is deploying can see the problem rather than a
// container that will not stay up.
func Load() (Config, error) {
	var cfg Config
	var err error

	setString(&cfg.Listen, "LABS_LISTEN", "")
	setString(&cfg.LogLevel, "LABS_LOG_LEVEL", "info")
	setString(&cfg.GitHubToken, "LABS_GITHUB_TOKEN", "")
	setString(&cfg.GitHubAPI, "LABS_GITHUB_API", "https://api.github.com")
	setString(&cfg.DispatchRef, "LABS_DISPATCH_REF", "main")
	setString(&cfg.DispatchSessionHours, "LABS_DISPATCH_SESSION_HOURS", "4")

	cfg.Listen = resolveListen(cfg.Listen)

	cfg.Repos = splitList(os.Getenv("LABS_REPOS"))

	// These four are the only unreadable values: the variable is set to
	// something that is not a duration or a number. A typo in one of them is
	// still a reason to stop, because continuing would silently substitute the
	// default for the value the operator clearly meant to set.
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
		// Malformed JSON is a problem, not a reason to stop: the rest of the
		// configuration may be perfectly usable, and the message says which
		// variable to fix.
		cfg.Envs = nil
		cfg.Problems = append(cfg.Problems, err.Error())
	}

	cfg.Problems = append(cfg.Problems, cfg.Validate()...)
	return cfg, nil
}

// Usable reports whether the configuration is complete enough to serve labs. It
// is what /readyz answers, and what decides whether the keeper is started.
func (c Config) Usable() bool { return len(c.Problems) == 0 }

// resolveListen decides the address to bind.
//
// LABS_LISTEN wins when it is set, because it is the variable this service
// documents and a deployment may want a specific interface. Failing that, PORT
// is honoured, because that is what a container platform — Fly, Render,
// Railway, Cloud Run, Heroku — injects and then probes to decide whether the
// service came up; a process that ignores it looks like one that never started.
// Failing that, :8080, which is the container's default and above the ports an
// unprivileged user can bind.
func resolveListen(labsListen string) string {
	if labsListen != "" {
		return labsListen
	}
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		return ":" + port
	}
	return ":8080"
}

// Validate lists what is wrong with the configuration.
//
// Every message names the variable at fault. A configuration error that only
// says what is wrong, not where, costs the reader a search through the deploy
// manifest for something the process already knew.
//
// It returns every problem rather than the first, so one pass shows everything
// that has to be set rather than one thing per restart.
func (c Config) Validate() []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if c.Listen == "" {
		add("LABS_LISTEN is empty")
	}
	if c.SessionTTL <= 0 {
		add("LABS_SESSION_TTL must be positive, got %s", c.SessionTTL)
	}
	if c.GitHubToken == "" {
		add("LABS_GITHUB_TOKEN is not set: the service dispatches workflows, which needs a token with Actions: write on the repositories in LABS_REPOS")
	}
	if len(c.Repos) == 0 {
		add("LABS_REPOS is not set: a comma-separated list of owner/repo the service may dispatch in")
	}
	for _, r := range c.Repos {
		if !strings.Contains(r, "/") {
			add("LABS_REPOS entry %q is not owner/repo", r)
		}
	}
	if !validSessionHours(c.DispatchSessionHours) {
		add("LABS_DISPATCH_SESSION_HOURS is %q, but the workflow declares it as a choice of 1, 2, 4 or unlimited", c.DispatchSessionHours)
	}
	if len(c.Envs) == 0 && !hasProblemPrefix(problems, "LABS_ENVIRONMENTS") {
		add("LABS_ENVIRONMENTS is not set: a JSON array describing at least one environment")
	}

	seen := make(map[string]bool, len(c.Envs))
	for _, e := range c.Envs {
		problems = append(problems, c.validateEnv(e)...)
		if seen[e.ID] {
			add("LABS_ENVIRONMENTS names %q twice", e.ID)
		}
		seen[e.ID] = true
	}
	return problems
}

// hasProblemPrefix reports whether a problem already mentions the variable, so
// a malformed value is not also reported as a missing one.
func hasProblemPrefix(problems []string, prefix string) bool {
	for _, p := range problems {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func (c Config) validateEnv(e model.Env) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	switch {
	case e.ID == "":
		add("LABS_ENVIRONMENTS has an environment with no id")
		return problems
	case !e.Kind.Known():
		add("LABS_ENVIRONMENTS entry %q has kind %q, which is not applab or sandboxlab", e.ID, e.Kind)
		return problems
	case e.Repo == "":
		add("LABS_ENVIRONMENTS entry %q names no repo", e.ID)
	case !contains(c.Repos, e.Repo):
		add("LABS_ENVIRONMENTS entry %q dispatches in %q, which is not in LABS_REPOS", e.ID, e.Repo)
	case e.Workflow == "":
		add("LABS_ENVIRONMENTS entry %q names no workflow", e.ID)
	case e.Ref == "":
		add("LABS_ENVIRONMENTS entry %q names no ref", e.ID)
	case !validHost(e.Domain):
		add("LABS_ENVIRONMENTS entry %q has domain %q, which must be a bare hostname with no scheme or path", e.ID, e.Domain)
	case !validBasePath(e.BasePath):
		add("LABS_ENVIRONMENTS entry %q has base_path %q, which must be empty or start with / and not end with one", e.ID, e.BasePath)
	case e.APIKey == "":
		add("environment %q has no key: set LABS_KEY_%s to the key that environment is configured with", e.ID, keyEnvSuffix(e.ID))
	case e.Capacity <= 0:
		add("LABS_ENVIRONMENTS entry %q has capacity %d, which must be positive", e.ID, e.Capacity)
	}

	switch e.Kind {
	case model.KindApplab:
		if len(e.Slots) == 0 {
			add("LABS_ENVIRONMENTS entry %q is an applab environment with no slots: it needs one app id per concurrent session", e.ID)
		} else if e.Capacity != len(e.Slots) {
			add("LABS_ENVIRONMENTS entry %q has capacity %d but %d slots; for applab they are the same number", e.ID, e.Capacity, len(e.Slots))
		}
	case model.KindSandboxlab:
		if e.Template == "" {
			add("LABS_ENVIRONMENTS entry %q is a sandboxlab environment with no template", e.ID)
		}
	}
	return problems
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
