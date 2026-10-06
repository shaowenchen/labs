package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The console page is a string, so nothing the compiler does reaches it: a Go
// word left in the JavaScript below is not a build error, it is a page that
// throws on its first line and renders blank. That happened — `func renderEnvs`
// sat in the embedded script and every check passed, because Go never parses
// what is inside a string literal and the HTTP tests only assert on the API.
//
// These two tests are the guard. The first asks whether there is a JavaScript
// runtime to hand; where there is one it is authoritative, since only a parser
// knows what the language allows. The second runs everywhere and is the cruder
// net: the page is not Go, and no line of it sets out to be a Go declaration.

// scriptOf returns the page's own script, which is the last one on it — the
// page has one, and taking the last keeps this right if a block is ever added
// in front of it.
func scriptOf(t *testing.T) string {
	t.Helper()
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(consoleHTML, -1)
	if len(blocks) == 0 {
		t.Fatal("the page carries no script at all")
	}
	return blocks[len(blocks)-1][1]
}

// TestConsoleScriptParses hands the page's script to a JavaScript parser.
//
// It is skipped where there is no runtime: this is a Go module, and a
// contributor without node installed must still be able to run the suite. Node
// is what the CI image and every machine that builds the page here has.
func TestConsoleScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node: cannot parse the page's script")
	}
	// A file rather than -e, so a parse error names a line that matches the
	// page and not one of a command string.
	path := filepath.Join(t.TempDir(), "page.js")
	if err := os.WriteFile(path, []byte(scriptOf(t)), 0o600); err != nil {
		t.Fatalf("write the script: %v", err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("the console page's script does not parse: %v\n%s", err, out)
	}
}

// TestConsoleScriptIsNotGo is the same check without a runtime.
//
// It looks for the one mistake that was actually made: a Go keyword opening a
// declaration, which is what happens when a function is edited in a Go file and
// written the way the surrounding code is. It cannot prove the script is valid;
// it catches the shape of the mistake that got through.
func TestConsoleScriptIsNotGo(t *testing.T) {
	decl := regexp.MustCompile(`(?m)^\s*func\s+[A-Za-z_$]`)
	if m := decl.FindString(scriptOf(t)); m != "" {
		t.Errorf("the page's script contains a Go declaration (%q). It is JavaScript: the body of console.go is a string, so the compiler never sees this line, and the page fails to parse and renders blank.", strings.TrimSpace(m))
	}
}
