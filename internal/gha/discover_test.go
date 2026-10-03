package gha

import "testing"

func TestExtractBaseURL(t *testing.T) {
	// A realistic slice of a debugger run's log: the tunnel host appears beside
	// GitHub's own addresses, and only one of them carries the base path.
	log := `
2026-10-02T12:00:00Z Cloning into 'applab'...
2026-10-02T12:03:00Z installed at https://api.github.com/repos/o/applab
2026-10-02T12:09:00Z tunnel up at https://random-words-1234.trycloudflare.com
2026-10-02T12:10:00Z waiting for the console at https://applab.chenshaowen.com/applab/healthz
2026-10-02T12:10:05Z ::notice title=AppLab is ready::https://applab.chenshaowen.com/applab
`
	got, ok := ExtractBaseURL(log, "/applab")
	if !ok {
		t.Fatal("no base URL found")
	}
	if got != "https://applab.chenshaowen.com" {
		t.Errorf("ExtractBaseURL = %q, want https://applab.chenshaowen.com", got)
	}
}

func TestExtractBaseURLIgnoresOtherHosts(t *testing.T) {
	// Without the base path there is nothing to key on, so the first https URL
	// wins — this is the degenerate case, and the point is only that it does not
	// crash or return a path.
	got, ok := ExtractBaseURL("see https://example.com/foo and https://bar.example.org", "/applab")
	if ok {
		t.Errorf("expected no match without the base path, got %q", got)
	}
}

func TestExtractBaseURLKeepsTheHostOnly(t *testing.T) {
	got, ok := ExtractBaseURL("ready: https://host.example.com/applab/apps/shop", "/applab")
	if !ok || got != "https://host.example.com" {
		t.Fatalf("got %q, %v; want the host with no path", got, ok)
	}
}

func TestExtractBaseURLFromANoticeLine(t *testing.T) {
	got, ok := ExtractBaseURL("::notice title=AppLab is ready::https://a.example.com/applab", "/applab")
	if !ok || got != "https://a.example.com" {
		t.Fatalf("got %q, %v", got, ok)
	}
}
