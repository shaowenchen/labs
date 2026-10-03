// Package config resolves the service's settings from the environment.
//
// Everything is an environment variable, set by the compose file or the chart.
// That keeps the binary free of a config file format and of flags that would
// have to be kept in step with the deployment, and it means the deployment's
// settings are readable from the pod spec — which is where someone debugging a
// deployment looks first.
//
// There is no environment list to write out. One entry in LABS_REPOS is one
// environment, and everything a repository implies — which project it runs, the
// path it is served under, the workflow that brings it up — is derived from its
// name. The only two things that cannot be derived are the domain it is served
// under and the key it is configured with, and those are one variable each:
// LABS_DOMAIN_<ID> and LABS_KEY_<ID>, where <ID> is the repository name
// uppercased.
package config

import (
	"crypto/rand"
	"encoding/hex"
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

	// DispatchRef forces the git ref workflows are dispatched on. Empty — the
	// default — means each repository's own default branch, which is what a
	// deployment almost always wants: the two projects publish from master and
	// from main, and a hardcoded ref dispatches one that does not exist on the
	// other, which is a 404 and no run.
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
	// as "owner/repo". It is also the environment list: one repository is one
	// environment.
	Repos []string

	// EnvSlots is how many concurrent sessions each environment serves, which
	// is the number of app ids it lends out. It applies to every environment.
	EnvSlots int

	// DomainSuffix is the domain environments are served under, as a bare
	// suffix: a repository named applab is served at applab.<suffix>. It is how
	// a deployment whose repositories follow that naming needs no per-environment
	// domain at all. Empty means every environment must name its own domain in
	// LABS_DOMAIN_<ID>.
	DomainSuffix string

	// Envs are the environments, one per repository, each with its key resolved.
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
	setString(&cfg.DispatchSessionHours, "LABS_DISPATCH_SESSION_HOURS", "4")
	setString(&cfg.DomainSuffix, "LABS_DOMAIN_SUFFIX", "")

	cfg.Listen = resolveListen(cfg.Listen)

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
	if cfg.EnvSlots, err = intEnv("LABS_ENV_SLOTS", defaultEnvSlots); err != nil {
		return Config{}, err
	}
	if cfg.KeepWarm, err = boolEnv("LABS_KEEPWARM", true); err != nil {
		return Config{}, err
	}
	if cfg.TrustedProxy, err = boolEnv("LABS_TRUSTED_PROXY", false); err != nil {
		return Config{}, err
	}

	cfg.Repos = splitList(os.Getenv("LABS_REPOS"))
	cfg.Envs = buildEnvs(cfg.Repos, cfg.EnvSlots, cfg.DomainSuffix, cfg.DispatchRef)

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
	if c.EnvSlots <= 0 {
		add("LABS_ENV_SLOTS is %d, but each environment needs at least one slot", c.EnvSlots)
	}

	// Ids come from repository names, so two repositories with the same name
	// under different owners would collide. It is unlikely and cheap to catch.
	seen := make(map[string]string, len(c.Envs))
	for _, e := range c.Envs {
		problems = append(problems, c.validateEnv(e)...)
		if prev, dup := seen[e.ID]; dup {
			add("repositories %q and %q have the same name, so their environments collide on %s", prev, e.Repo, e.ID)
		}
		seen[e.ID] = e.Repo
	}
	return problems
}

func (c Config) validateEnv(e model.Env) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	switch {
	case e.Workflow == "":
		add("environment %q names no workflow", e.ID)
	case !validBasePath(e.BasePath):
		add("environment %q has base_path %q, which must be empty or start with / and not end with one", e.ID, e.BasePath)
	case e.Domain != "" && !validHost(e.Domain):
		add("environment %q has domain %q, which must be a bare hostname with no scheme or path (or leave it out and let the address be read from the environment's own run log)", e.ID, e.Domain)
	case e.APIKey == "":
		add("environment %q has no key: set LABS_KEY_%s to the key that environment is configured with", e.ID, e.ID)
	case e.Capacity <= 0:
		add("environment %q has capacity %d, which must be positive", e.ID, e.Capacity)
	}

	if e.Kind == model.KindApplab {
		if len(e.Slots) == 0 {
			add("environment %q is an applab environment with no slots: set LABS_ENV_SLOTS to at least 1", e.ID)
		} else if e.Capacity != len(e.Slots) {
			add("environment %q has capacity %d but %d slots; for applab they are the same number", e.ID, e.Capacity, len(e.Slots))
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

// defaultEnvSlots is how many concurrent sessions each environment serves when
// LABS_ENV_SLOTS does not say: eight app slots, which is how many applications
// one applab cluster is expected to carry at once. Each one is a running app in
// the cluster, so raising it raises what the cluster holds.
const defaultEnvSlots = 8

// basePathFor is the path a project's whole deployment is served under. It is a
// function of the project, not a setting: applab is served under /applab and
// sandboxlab under /sandbox, and a deployment that changed it in only one of
// the two places — here and the chart — would poll an address nothing answers.
func basePathFor(kind model.Kind) string {
	switch kind {
	case model.KindApplab:
		return "/applab"
	case model.KindSandboxlab:
		return "/sandbox"
	default:
		return ""
	}
}

// buildEnvs turns the repository list into the environments this deployment
// runs: one repository is one environment.
//
// Everything a repository implies is derived from its name — which project it
// is, the path it is served under, the workflow that brings it up. The key is
// generated when it is not configured, and handed to the environment through
// the dispatch, so the common deployment needs only LABS_REPOS and a token.
// The one thing that cannot be derived is the domain each environment is served
// under, because it is the hostname of the tunnel that deployment happens to
// own; it is the repository name under a shared suffix
// (applab.<LABS_DOMAIN_SUFFIX>), or LABS_DOMAIN_<ID> for one that does not fit
// that shape.
func buildEnvs(repos []string, slots int, domainSuffix, ref string) []model.Env {
	envs := make([]model.Env, 0, len(repos))
	for _, repo := range repos {
		kind := kindFor(repo)
		name := repoName(repo)
		id := envID(name)

		domain := strings.TrimSpace(os.Getenv("LABS_DOMAIN_" + id))
		if domain == "" && domainSuffix != "" {
			domain = strings.ToLower(name) + "." + domainSuffix
		}

		env := model.Env{
			ID:       id,
			Kind:     kind,
			Repo:     repo,
			Workflow: workflowFor(kind),
			Ref:      ref,
			Scheme:   "https",
			Domain:   domain,
			BasePath: basePathFor(kind),
			Template: strings.TrimSpace(os.Getenv("LABS_TEMPLATE_" + id)),
			Slots:    slotNames(slots),
			Capacity: slots,
			APIKey:   strings.TrimSpace(os.Getenv("LABS_KEY_" + id)),
		}
		// applab accepts an api_key input, so a key labs generates can be handed
		// to the environment at dispatch — which is what lets an applab
		// deployment run on a token and a repository list alone. sandboxlab's
		// workflow takes no such input, so its key must be configured; a
		// generated one could never reach the environment, and validateEnv says
		// so.
		if env.APIKey == "" && env.Kind == model.KindApplab {
			key, err := generateKey()
			if err != nil {
				// crypto/rand failing is not something to continue past: every
				// environment would then share one key.
				panic("config: generating an API key: " + err.Error())
			}
			env.APIKey = key
			env.ManagedKey = true
		}
		envs = append(envs, env)
	}
	return envs
}

// repoName is the part of "owner/name" after the slash, or the whole entry when
// there is no slash.
func repoName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// kindFor names the project a repository belongs to, from its name: the two
// debugger environments this service drives are named applab and sandboxlab.
// A repository named neither is treated as applab, which is the kind that is
// implemented; the mismatch shows up as a domain that never answers.
func kindFor(repo string) model.Kind {
	if strings.Contains(strings.ToLower(repoName(repo)), "sandbox") {
		return model.KindSandboxlab
	}
	return model.KindApplab
}

// workflowFor is the workflow file a project's debugger environment is brought
// up by. Both projects name it debugger.yml.
func workflowFor(kind model.Kind) string {
	return "debugger.yml"
}

// envID turns a repository name into an environment id, and the tail of the
// variables named for it: "applab" becomes "APPLAB", so the variables are
// LABS_DOMAIN_APPLAB and LABS_KEY_APPLAB.
func envID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// slotNames is the app ids an environment lends out: lab-01, lab-02, ...
func slotNames(n int) []string {
	if n < 0 {
		n = 0
	}
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("lab-%02d", i))
	}
	return out
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

// generateKey mints an environment key. It is 32 hex characters — 16 bytes —
// long enough that guessing is not a threat model. It is generated once at
// startup and lives only in memory and in the dispatches that carry it, so it
// changes on every restart; that is why the environment is told it at dispatch
// time rather than being configured with it out of band.
func generateKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
