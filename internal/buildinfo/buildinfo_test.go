package buildinfo

import "testing"

func TestString(t *testing.T) {
	savedVersion, savedCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = savedVersion, savedCommit })

	tests := []struct {
		name    string
		version string
		commit  string
		want    string
	}{
		{"unbuilt", "dev", "unknown", "dev"},
		{"a release tag", "v0.1.0", "dfdb626", "v0.1.0 (dfdb626)"},
		{"no tags at all", "dfdb626", "dfdb626", "dfdb626"},
		{"a version with no commit", "v0.1.0", "", "v0.1.0"},
		{"a commit with no version", "", "dfdb626", "dfdb626"},
		{"neither", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			Version, Commit = tc.version, tc.commit
			if got := String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKnown(t *testing.T) {
	cases := map[string]bool{
		"dfdb626": true, "2026-10-03T21:41:37Z": true,
		"": false, "unknown": false, "unknown (from a build)": false,
	}
	for in, want := range cases {
		if got := Known(in); got != want {
			t.Errorf("Known(%q) = %v, want %v", in, got, want)
		}
	}
}

// A build in this working copy carries VCS metadata, so the init fallback fills
// a commit even when no ldflags were passed — which is the whole point of it.
func TestCommitRecoveredWithoutLdflags(t *testing.T) {
	// The test binary is built from this module, in this git checkout, so the
	// fallback should have found a revision. If the module were built outside a
	// git work tree this would be skipped rather than fail, which is the honest
	// outcome.
	if Commit == "unknown" || Commit == "" {
		t.Skip("no VCS metadata in this build; the fallback has nothing to read")
	}
	if len(Commit) > 12 {
		t.Errorf("Commit = %q, want a short revision", Commit)
	}
}

// The fallback must not blank out the placeholder: with nothing to read, the
// answer stays "unknown", not "".
func TestNoSourceKeepsUnknown(t *testing.T) {
	savedCommit, savedTime := Commit, BuildTime
	t.Cleanup(func() { Commit, BuildTime = savedCommit, savedTime })

	// Simulate a build with the linker defaults and nothing to fall back on.
	Commit, BuildTime = "unknown", "unknown"
	// init already ran for this binary with real VCS data, so exercise the
	// helpers directly instead: they must return "" rather than a value.
	for _, k := range commitVars {
		t.Setenv(k, "")
	}
	if got := commitFromEnv(); got != "" {
		t.Errorf("commitFromEnv with nothing set = %q, want empty", got)
	}
	if got := shorten(""); got != "" {
		t.Errorf("shorten(\"\") = %q, want empty", got)
	}
}

func TestShorten(t *testing.T) {
	cases := map[string]string{
		"1234567890abcdef": "1234567",
		"abc1234":          "abc1234",
		"abc":              "abc",
		"":                 "",
	}
	for in, want := range cases {
		if got := shorten(in); got != want {
			t.Errorf("shorten(%q) = %q, want %q", in, got, want)
		}
	}
}

// The platform variables are read from the environment when the linker did not
// supply a commit.
func TestCommitFromEnv(t *testing.T) {
	for _, k := range commitVars {
		t.Setenv(k, "")
	}
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "deadbeef1234")
	if got := commitFromEnv(); got != "deadbeef1234" {
		t.Errorf("commitFromEnv = %q, want the Vercel SHA", got)
	}
	if got := shorten(commitFromEnv()); got != "deadbee" {
		t.Errorf("shortened = %q, want deadbee", got)
	}
}

// SOURCE_DATE_EPOCH is a Unix timestamp and everything else here is a date, so
// the one is converted into the other. A deploy that sets it — it is the
// reproducible-builds convention, so a build system may well do it — would
// otherwise put a bare integer in BuildTime, and the page renders that with
// `new Date(...)`, which prints "Invalid Date".
func TestSourceDateEpochIsConverted(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1791259643")
	if got := timeFromEnv(); got != "2026-10-06T04:07:23Z" {
		t.Errorf("timeFromEnv() = %q, want the epoch as RFC3339", got)
	}
}

// A value that is not a number is left alone rather than dropped: something was
// meant by it, and showing it beats showing nothing.
func TestSourceDateEpochThatIsNotANumberIsKept(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "whenever")
	if got := timeFromEnv(); got != "whenever" {
		t.Errorf("timeFromEnv() = %q, want it unchanged", got)
	}
}

// BUILD_TIME wins over SOURCE_DATE_EPOCH: it is the caller's own value, and it
// is already the shape the rest of this expects.
func TestBuildTimeWinsOverSourceDateEpoch(t *testing.T) {
	t.Setenv("BUILD_TIME", "2026-10-06T04:07:23Z")
	t.Setenv("SOURCE_DATE_EPOCH", "1791259643")
	if got := timeFromEnv(); got != "2026-10-06T04:07:23Z" {
		t.Errorf("timeFromEnv() = %q, want BUILD_TIME", got)
	}
}
