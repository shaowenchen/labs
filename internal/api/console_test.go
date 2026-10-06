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

// TestConsoleStringsAreTranslated guards the second invisible mistake this page
// can make. A language table that has drifted from the code does not fail to
// build and does not render blank: t() falls back to the key, so the page shows
// "row.console" where it meant to show a word. It is only visible in the one
// language nobody testing the page reads.
//
// The keys are read out of the calls to t('...'), and every one must be present
// in every table — English, and each translation beside it.
func TestConsoleStringsAreTranslated(t *testing.T) {
	script := scriptOf(t)

	// The tables themselves: the keys each language defines.
	langs := map[string]map[string]bool{}
	for _, m := range regexp.MustCompile(`(?ms)^  ([a-z]{2}): \{\n(.*?)^  \},\n`).FindAllStringSubmatch(script, -1) {
		keys := map[string]bool{}
		for _, k := range regexp.MustCompile(`'([a-z][\w.]*)':`).FindAllStringSubmatch(m[2], -1) {
			keys[k[1]] = true
		}
		langs[m[1]] = keys
	}
	if len(langs["en"]) == 0 {
		t.Fatal("no English string table found")
	}
	// The values the tables carry can be checked for holes: a string that means
	// to be filled in but has no placeholder would print a brace to the reader.
	for _, lang := range []string{"zh"} {
		if len(langs[lang]) == 0 {
			t.Errorf("the %s table is missing or empty", lang)
		}
	}

	// The keys the page actually asks for. A call may be written with the key
	// inline or through a ternary, so both spellings are collected.
	used := map[string]bool{}
	for _, k := range regexp.MustCompile(`\bt\(\s*'([\w.]+)'`).FindAllStringSubmatch(script, -1) {
		used[k[1]] = true
	}
	if len(used) < 20 {
		t.Fatalf("found only %d translated strings; the scan has probably broken", len(used))
	}
	for k := range used {
		for lang, keys := range langs {
			if !keys[k] {
				t.Errorf("the %s table has no %q, which the page asks for; it would show the key itself", lang, k)
			}
		}
	}
	for lang, keys := range langs {
		for k := range keys {
			if !used[k] {
				t.Errorf("the %s table defines %q, which nothing asks for", lang, k)
			}
		}
	}

	// A translation must fill in the same placeholders as the English. One that
	// drops {h} prints "小时12分" with the hour missing and no sign that anything
	// was left out — the numbers are all still numbers.
	values := map[string]map[string]string{}
	for _, m := range regexp.MustCompile(`(?ms)^  ([a-z]{2}): \{\n(.*?)^  \},\n`).FindAllStringSubmatch(script, -1) {
		vals := map[string]string{}
		for _, kv := range regexp.MustCompile(`'([a-z][\w.]*)': '([^']*)'`).FindAllStringSubmatch(m[2], -1) {
			vals[kv[1]] = kv[2]
		}
		values[m[1]] = vals
	}
	placeholder := regexp.MustCompile(`\{(\w+)\}`)
	for k, en := range values["en"] {
		want := map[string]bool{}
		for _, m := range placeholder.FindAllStringSubmatch(en, -1) {
			want[m[1]] = true
		}
		for lang, vals := range values {
			if lang == "en" {
				continue
			}
			got := map[string]bool{}
			for _, m := range placeholder.FindAllStringSubmatch(vals[k], -1) {
				got[m[1]] = true
			}
			for name := range want {
				if !got[name] {
					t.Errorf("the %s %q drops the {%s} that the English has (%q); it would print with that value missing", lang, k, name, en)
				}
			}
			for name := range got {
				if !want[name] {
					t.Errorf("the %s %q has a {%s} the English does not (%q)", lang, k, name, en)
				}
			}
		}
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
