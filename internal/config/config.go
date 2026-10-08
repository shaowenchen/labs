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
// name. The address is derived too, defaulting to the hostname each project's
// own workflow declares as a choice. What is left is one key, shared by every
// environment when none of them names its own: ADMIN_KEY, or
// LABS_KEY_<ID> for the one environment that wants its own, where <ID> is the
// repository name uppercased.
//
// The key is genuinely optional, and leaving it out is not the same as
// defaulting it: there is no built-in value, and none is generated. A key
// checked into this repository is not a secret, and a generated one is a
// different value on every restart, so neither is a value a deployment could
// rely on. Left unset, there is simply no key — it is not sent in a dispatch,
// and it is not attached to a call, so labs and its environments meet with no
// credential at all, which is what a deployment that sets none on either side
// runs as. Set it, to the value the repositories are given, to call an
// environment that asks for one.
package config

import (
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
	// every repository in Repos. It comes from GITHUB_TOKEN, which is the name a
	// CI environment already sets, so a deployment on a runner needs no second
	// copy of the same credential.
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
	//
	// Each kind has its own variable, so configuring one kind can never hide the
	// other: LABS_APPLAB_REPOS and LABS_SANDBOXLAB_REPOS. A single list covering
	// both was the earlier shape, and it meant setting it to one repository
	// silently dropped the other kind — a deployment that looks configured and
	// is quietly serving half of what it should.
	Repos []string

	// Specs is what Repos is built from, reached one step earlier: each entry
	// with the kind it runs and the variable that named it. Validation uses it
	// to point a bad entry at the variable to fix.
	Specs []envSpec

	// ReposSource says, per kind, where that kind's repository list came from:
	// the variable that set it, "default", or "off" for a kind turned off by
	// setting its variable to empty. It is reported so a deployment serving
	// fewer kinds than expected can see which variable decided that.
	ReposSource map[string]string

	// EnvSlots is how many concurrent sessions each environment serves. It
	// applies to every environment. The name is a holdover from when an applab
	// environment lent out a fixed pool of app ids of this size; the ids are
	// minted per session now, so this is a count.
	EnvSlots int

	// DomainSuffix is the domain environments are served under, as a bare
	// suffix: a repository named applab is served at applab.<suffix>. It is how
	// a deployment whose repositories follow that naming needs no per-environment
	// domain at all. Empty means every environment must name its own domain in
	// LABS_DOMAIN_<ID>.
	DomainSuffix string

	// APIKey is the key every environment takes when it has none of its own, from
	// ADMIN_KEY. It is the one key a deployment is called with, rather
	// than one per repository. Empty is a legitimate value, not a missing one:
	// nothing is generated in its place, so an unset key means calls are made
	// with no credential at all. See the package comment.
	APIKey string

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
	setString(&cfg.GitHubToken, "GITHUB_TOKEN", "")
	setString(&cfg.GitHubAPI, "LABS_GITHUB_API", "https://api.github.com")
	setString(&cfg.DispatchSessionHours, "LABS_DISPATCH_SESSION_HOURS", "4")
	setString(&cfg.DomainSuffix, "LABS_DOMAIN_SUFFIX", "")
	setString(&cfg.APIKey, "ADMIN_KEY", "")

	cfg.Listen = resolveListen(cfg.Listen)

	// These are the only unreadable values: the variable is set to something
	// that is not a duration or a number. A typo in one of them is still a
	// reason to stop, because continuing would silently substitute the default
	// for the value the operator clearly meant to set.
	if cfg.SessionTTL, err = durationEnv("LABS_SESSION_TTL", 2*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.ReapInterval, err = durationEnv("LABS_REAP_INTERVAL", 30*time.Second); err != nil {
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
	if cfg.TrustedProxy, err = boolEnv("LABS_TRUSTED_PROXY", false); err != nil {
		return Config{}, err
	}

	// One variable per kind, so configuring one kind can never hide the other.
	// A single LABS_REPOS covering both was the earlier shape and it did exactly
	// that: setting it to one repository silently dropped the other kind.
	//
	// Which variable a repository comes from is also what says which kind it is.
	// That is deliberate: the kind decides the served path, the workflow and the
	// driver, so getting it wrong is not a smaller mistake than a typo — and
	// deriving it from the repository's name meant a fork called something else
	// was silently driven as the wrong project.
	cfg.ReposSource = map[string]string{}
	var specs []envSpec
	for _, k := range []struct {
		kind model.Kind
		env  string
	}{
		{model.KindApplab, "LABS_APPLAB_REPOS"},
		{model.KindSandboxlab, "LABS_SANDBOXLAB_REPOS"},
	} {
		raw, set := os.LookupEnv(k.env)
		switch {
		case !set || strings.TrimSpace(raw) == "":
			// Unset takes the default repository. Set to empty is not the same
			// thing — see below.
			if set {
				cfg.ReposSource[string(k.kind)] = "off"
				continue
			}
			cfg.ReposSource[string(k.kind)] = "default"
			specs = append(specs, envSpec{kind: k.kind, repo: defaultRepoFor(k.kind), source: k.env})
		default:
			cfg.ReposSource[string(k.kind)] = k.env
			for _, repo := range splitList(raw) {
				specs = append(specs, envSpec{kind: k.kind, repo: repo, source: k.env})
			}
		}
	}
	cfg.Specs = specs
	for _, s := range specs {
		cfg.Repos = append(cfg.Repos, s.repo)
	}
	cfg.Envs = buildEnvs(specs, cfg.EnvSlots, cfg.DomainSuffix, cfg.DispatchRef, cfg.APIKey)

	cfg.Problems = append(cfg.Problems, cfg.Validate()...)
	return cfg, nil
}

// Usable reports whether the configuration is complete enough to serve labs. It
// is what /readyz answers, and whether an environment can be dispatched at all.
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
		add("GITHUB_TOKEN is not set: the service dispatches workflows, which needs a token with Actions: write on the repositories it runs")
	}
	// Each repository is named with the variable it came from, so a bad entry
	// points at the variable to fix rather than at whichever list it landed in.
	for _, s := range c.Specs {
		if !strings.Contains(s.repo, "/") {
			add("%s entry %q is not owner/repo", s.source, s.repo)
		}
	}
	// Both kinds turned off is a deployment that can serve nothing, which is
	// worth naming: it is where "no environment is available" comes from.
	if len(c.Envs) == 0 {
		add("no environment is configured: set LABS_APPLAB_REPOS or LABS_SANDBOXLAB_REPOS, or leave both unset for the defaults")
	}
	if !validSessionHours(c.DispatchSessionHours) {
		add("LABS_DISPATCH_SESSION_HOURS is %q, but the workflow declares it as a choice of 1, 2, 4 or unlimited", c.DispatchSessionHours)
	}
	if c.EnvSlots <= 0 {
		add("LABS_ENV_SLOTS is %d, but each environment must serve at least one session", c.EnvSlots)
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
	case e.Capacity <= 0:
		add("environment %q has capacity %d, which must be positive", e.ID, e.Capacity)
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
// LABS_ENV_SLOTS does not say: eight, which is how many applications one applab
// cluster is expected to carry at once. The name is left over from when this was
// a pool of pre-made app ids; it is a count of concurrent sessions now, which is
// what it always meant to an operator.
const defaultEnvSlots = 8

// basePathFor is the path a project's whole deployment is served under by
// default: applab is served under /applab and sandboxlab under /sandboxlab.
//
// It was fixed rather than settable, on the reasoning that the path is a
// property of the project. It is not — the deployment charts set it, and one of
// them now serves sandboxlab under /sandboxlab — so this is a default, and
// LABS_BASE_PATH_<ID> overrides it exactly as LABS_DOMAIN_<ID> overrides the
// hostname. A wrong guess here is not a missing feature but a dead environment:
// every probe and every provision goes to an address nothing answers, and the
// page reports the environment as not up.
func basePathFor(kind model.Kind) string {
	switch kind {
	case model.KindApplab:
		return "/applab"
	case model.KindSandboxlab:
		return "/sandboxlab"
	default:
		return ""
	}
}

// buildEnvs turns the repository list into the environments this deployment
// runs: one repository is one environment.
//
// Everything a repository implies is derived from its name — which project it
// is, the path it is served under, the workflow that brings it up. The key is
// optional and is handed to the environment through the dispatch when one is
// configured, so the common deployment needs only LABS_REPOS and a token.
// The one thing that cannot be derived is the domain each environment is served
// under, because it is the hostname of the tunnel that deployment happens to
// own; it is the repository name under a shared suffix
// (applab.<LABS_DOMAIN_SUFFIX>), or LABS_DOMAIN_<ID> for one that does not fit
// that shape.
// defaultDomains are the hostnames an environment falls back to when none is
// configured. Each is a value the project's own debugger workflow declares as a
// choice — so it is always one the dispatch will accept — and a named tunnel is
// expected to serve it. They are defaults, not the shape of a deployment: a
// different tunnel is pointed at with LABS_DOMAIN_<ID> or LABS_DOMAIN_SUFFIX.
var defaultDomains = map[model.Kind]string{
	model.KindApplab:     "applab-1.chenshaowen.com",
	model.KindSandboxlab: "sandboxlab-1.chenshaowen.com",
}

// envSpec is one environment to build: which kind, the repository that runs it,
// and the variable that named it — for error messages that point at the thing
// to fix. The kind is carried rather than derived from the repository's name,
// because the variable it came from is what decided it.
type envSpec struct {
	kind   model.Kind
	repo   string
	source string
}

// buildEnvs turns the repository list into environments. sharedKey is the key
// an environment with none of its own is called with, from ADMIN_KEY.
//
// It may be empty, and that is a deployment with no key: the value then goes
// nowhere — no api_key in a dispatch, no credential on a call — which is exactly
// what leaving ADMIN_KEY unset asks for. It is resolved here, once, rather than
// per request so that the dispatch and the calls that follow it agree on the
// same value, whether that value is a key or nothing.
func buildEnvs(specs []envSpec, capacity int, domainSuffix, ref, sharedKey string) []model.Env {
	envs := make([]model.Env, 0, len(specs))
	for _, spec := range specs {
		kind, repo := spec.kind, spec.repo
		name := repoName(repo)
		id := envID(name)

		// The address, in order: a per-environment override, then a shared
		// suffix (repository name under it), then the project's own default.
		domain := strings.TrimSpace(os.Getenv("LABS_DOMAIN_" + id))
		if domain == "" && domainSuffix != "" {
			domain = strings.ToLower(name) + "." + domainSuffix
		}
		if domain == "" {
			domain = defaultDomains[kind]
		}

		// The path under the hostname, overridden the same way. A deployment
		// whose chart serves the project somewhere other than the default would
		// otherwise be probed and provisioned at an address nothing answers.
		basePath := strings.TrimSpace(os.Getenv("LABS_BASE_PATH_" + id))
		if basePath == "" {
			basePath = basePathFor(kind)
		}

		env := model.Env{
			ID:       id,
			Kind:     kind,
			Repo:     repo,
			Workflow: workflowFor(kind),
			Ref:      ref,
			Scheme:   "https",
			Domain:   domain,
			BasePath: basePath,
			Template: strings.TrimSpace(os.Getenv("LABS_TEMPLATE_" + id)),
			Capacity: capacity,
			APIKey:   strings.TrimSpace(os.Getenv("LABS_KEY_" + id)),
		}
		if env.APIKey == "" {
			// One key for every environment rather than one minted per dispatch, and
			// no key at all when none is configured: it is the same value across
			// every restart of this process, so labs can call whatever environment is
			// actually up, including one it did not itself just dispatch. ADMIN_KEY
			// is that key when it is set; when it is not, this stays empty and calls
			// carry no credential.
			env.APIKey = sharedKey
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

// defaultRepos are the repositories this service drives when neither per-kind
// variable says anything: one entry each, the two projects whose debugger
// environments it hands out. Set LABS_APPLAB_REPOS or LABS_SANDBOXLAB_REPOS to
// run a fork, or to run only one of the two kinds.
var defaultRepos = map[model.Kind]string{
	model.KindApplab:     "shaowenchen/applab",
	model.KindSandboxlab: "shaowenchen/sandboxlab",
}

func defaultRepoFor(k model.Kind) string { return defaultRepos[k] }
