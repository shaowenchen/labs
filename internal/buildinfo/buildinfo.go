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
	"runtime/debug"
	"strings"
)

// Version is the release version, or "dev" for a build that was not tagged.
var Version = "dev"

// Commit is the git revision the binary was built from, or "unknown".
var Commit = "unknown"

// BuildTime is when the binary was built, as an RFC3339 UTC timestamp, or
// "unknown". It is when the build happened, not when the process started, so a
// page that shows it is answering "how current is this deployment".
var BuildTime = "unknown"

// init fills anything the linker did not, from the module's build metadata.
//
// It runs after the package variables are set and after the linker has had its
// say, so it only ever supplies what is still missing.
func init() {
	if Commit != "" && Commit != "unknown" && BuildTime != "" && BuildTime != "unknown" {
		return
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
		Commit = settings["vcs.revision"]
		if len(Commit) > 7 {
			Commit = Commit[:7]
		}
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
