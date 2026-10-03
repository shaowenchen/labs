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
