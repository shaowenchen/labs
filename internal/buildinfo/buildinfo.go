// Package buildinfo carries the version of the running binary.
//
// The values are injected at link time (`-X .../buildinfo.Version=...`) by the
// Dockerfile and the Makefile, so a binary built with neither still reports
// something honest rather than an empty string.
//
// When they are not injected — a build system that does not pass ldflags, which
// is how a host that builds the source itself behaves — the commit and time are
// recovered from Go's own build metadata instead. `go build` records the VCS
// revision and commit time into the binary when it is built in a git working
// copy, and runtime/debug exposes them, so the answer survives a build that was
// never told to carry it.
package buildinfo

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Version is the release version, or "dev" for a build that was not tagged.
var Version = "dev"

// Commit is the git revision the binary was built from, or "unknown".
var Commit = "unknown"

// BuildTime is when the binary was built, as an RFC3339 UTC timestamp, or
// "unknown". It is when the build happened, not when the process started, so a
// page that shows it is answering "how current is this deployment".
var BuildTime = "unknown"

// init fills anything the linker did not.
//
// Two sources, in order. Environment variables first, because a host that
// builds the source itself usually knows the revision it is building and passes
// it in one of the well-known variables below — and it may build from an
// archive with no .git, which is exactly when the second source has nothing to
// say. Then Go's own build metadata, which `go build` records when it runs in a
// git working copy.
func init() {
	if Commit != "" && Commit != "unknown" && BuildTime != "" && BuildTime != "unknown" {
		return
	}

	if Commit == "" || Commit == "unknown" {
		// Only assign when something was found: assigning an empty string would
		// replace the "unknown" placeholder with nothing, and the page would
		// show a blank where "unknown" is the honest answer.
		if sha := commitFromEnv(); sha != "" {
			Commit = shorten(sha)
		}
	}
	if BuildTime == "" || BuildTime == "unknown" {
		if t := timeFromEnv(); t != "" {
			BuildTime = t
		}
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	settings := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}

	if (Commit == "" || Commit == "unknown") && settings["vcs.revision"] != "" {
		Commit = shorten(settings["vcs.revision"])
		if settings["vcs.modified"] == "true" {
			Commit += "-dirty"
		}
	}
	// vcs.time is the commit's timestamp, not the build's. Go records no build
	// wall clock, so on a build with no injected time this is the closest honest
	// answer available — it says how current the code is, which is what the
	// reader of a version string wants.
	if (BuildTime == "" || BuildTime == "unknown") && settings["vcs.time"] != "" {
		BuildTime = settings["vcs.time"]
	}
}

// commitVars are the environment variables platforms use to hand a build the
// revision it is building. Vercel's is first: it builds from a snapshot with no
// .git, so this is the only place the commit exists there.
var commitVars = []string{
	"VERCEL_GIT_COMMIT_SHA",
	"GITHUB_SHA",
	"CI_COMMIT_SHA",  // GitLab
	"SOURCE_VERSION", // Heroku
	"COMMIT_SHA",
	"GIT_COMMIT",
	"REVISION",
}

func commitFromEnv() string {
	for _, name := range commitVars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// timeFromEnv looks for a build timestamp in the variables a platform might
// provide. Few do, so it usually returns nothing and the VCS commit time or
// "unknown" stands.
//
// SOURCE_DATE_EPOCH is the odd one out: it is a Unix timestamp, not a date, and
// it is the one of the three with a real convention behind it. It is converted
// rather than passed through, because what leaves here is rendered as a date —
// the page does `new Date(build_time)` — and a bare integer would print as
// "Invalid Date" where the footer is supposed to say when the build happened.
func timeFromEnv() string {
	for _, name := range []string{"BUILD_TIME", "BUILD_TIMESTAMP"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(os.Getenv("SOURCE_DATE_EPOCH")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(secs, 0).UTC().Format(time.RFC3339)
		}
		// Not a number: left as it came, rather than dropped. Something was
		// meant by it, and showing it beats showing nothing.
		return v
	}
	return ""
}

// shorten trims a revision to the seven characters git itself abbreviates to,
// leaving anything shorter alone.
func shorten(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

// String renders the version and commit as one line, for logs and /api/v1/config.
//
// The two are usually the same string on a build with no tag, because
// `git describe --tags --always` falls back to the commit — so the duplicate is
// dropped rather than printed as "dfdb626 (dfdb626)".
func String() string {
	switch {
	case Version == "":
		return Commit
	case Commit == "" || Commit == "unknown" || Commit == Version:
		return Version
	default:
		return Version + " (" + Commit + ")"
	}
}

// Known reports whether a value is worth showing: not empty, and not the
// placeholder a build that carried nothing falls back to.
func Known(v string) bool {
	return v != "" && v != "unknown" && !strings.HasPrefix(v, "unknown")
}
