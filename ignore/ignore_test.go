package ignore

import (
	"errors"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestMatcherFollowsGitignore(t *testing.T) {
	cases := []struct {
		patterns string
		name     string
		dir      bool
		want     bool
	}{
		{"*.log", "a.log", false, true},
		{"*.log", "deep/dir/a.log", false, true},
		{"*.log", "a.log.txt", false, false},
		{"# comment\n\n*.log", "# comment", false, false},
		{`\#hash`, "#hash", false, true},
		{`\!bang`, "!bang", false, true},
		{"build/", "build", true, true},
		{"build/", "build", false, false},
		{"build/", "src/build", true, true},
		{"build/", "build/out.o", false, true},
		{"/root.txt", "root.txt", false, true},
		{"/root.txt", "sub/root.txt", false, false},
		{"docs/*.md", "docs/a.md", false, true},
		{"docs/*.md", "x/docs/a.md", false, false},
		{"docs/*.md", "docs/sub/a.md", false, false},
		{"**/cache", "cache", true, true},
		{"**/cache", "a/b/cache", true, true},
		{"a/**/b", "a/b", false, true},
		{"a/**/b", "a/x/y/b", false, true},
		{"logs/**", "logs", true, false},
		{"logs/**", "logs/x", false, true},
		{"logs/**", "logs/x/y", false, true},
		{"?.txt", "a.txt", false, true},
		{"?.txt", "ab.txt", false, false},
		{"[abc].txt", "b.txt", false, true},
		{"[a-c].txt", "d.txt", false, false},
		{"[!a-c].txt", "d.txt", false, true},
		{"[^a-c].txt", "a.txt", false, false},
		{"[]x].txt", "].txt", false, true},
		{"*.tmp\n!keep.tmp", "keep.tmp", false, false},
		{"*.tmp\n!keep.tmp", "drop.tmp", false, true},
		{"!keep.tmp\n*.tmp", "keep.tmp", false, true},
		{"build/\n!build/keep", "build/keep", false, true},
		{"build/*\n!build/keep", "build/keep", false, false},
		{"build/*\n!build/keep", "build", true, false},
		{"trailing   ", "trailing", false, true},
		{`space\ `, "space ", false, true},
		{"crlf\r", "crlf", false, true},
		{"a**b", "axxb", false, true},
		{"é?", "éü", false, true},
		{"/", "anything", false, false},
	}
	for _, c := range cases {
		m, err := Parse([]byte(c.patterns))
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.patterns, err)
		}
		if got := m.Ignored(c.name, c.dir); got != c.want {
			t.Errorf("%q on %q (dir %v) = %v, want %v", c.patterns, c.name, c.dir, got, c.want)
		}
	}
}

func TestMalformedBodiesDoNotParse(t *testing.T) {
	for _, body := range []string{"[abc", `trailing\`, "a\x00b", "\xff", "[!]", "a//b"} {
		if _, err := Parse([]byte(body)); !errors.Is(err, ErrPattern) {
			t.Errorf("Parse(%q) = %v, want ErrPattern", body, err)
		}
	}
}

// The contract validates fallback patterns without this package; both must
// accept exactly the same single-line syntax.
func TestContractAgreesOnPatternSyntax(t *testing.T) {
	corpus := []string{
		"", " ", "#", "# note", `\#`, "!", "!x", `\!x`, "/", "//", "a/", "a//", "/a", "//a", "a//b", "a/b/",
		"**", "**/", "a/**", "**/a", "a**b", "*", "?", "[", "]", "[]", "[]]", "[a]", "[!a]", "[^a]", "[!]", "[!]]",
		"[a-z]", `[\]]`, `[\`, `a\`, `a\\`, `a\ `, "a ", `a\  `, "[a]/[b", "é", "\xff", "a\x00", "a\rb", "a\n",
		"[[]", "[a/b]", `x/\`, "a\t",
	}
	for _, p := range corpus {
		_, compileErr := Compile([]string{p})
		compiles := compileErr == nil && !strings.ContainsAny(p, "\r")
		valid := execenv.ValidateIgnoreRules(execenv.IgnoreRules{File: "rules", Fallback: []string{p}}) == nil
		if compiles != valid {
			t.Errorf("pattern %q: matcher accepts %v, contract accepts %v", p, compiles, valid)
		}
	}
}

func TestRulesFallBackAndReparseOnChange(t *testing.T) {
	r, err := New("rules", []string{"*.o"})
	if err != nil {
		t.Fatal(err)
	}
	if p := r.Policy(nil, false); p.Observed("a.o", false, false) {
		t.Fatal("missing file ignored the fallback")
	}
	if p := r.Policy([]byte("[bad"), true); p.Observed("a.o", false, false) {
		t.Fatal("unparseable file ignored the fallback")
	}
	if p := r.Policy([]byte(strings.Repeat("#", MaxFileBytes+1)), true); p.Observed("a.o", false, false) {
		t.Fatal("oversized file ignored the fallback")
	}
	p := r.Policy([]byte("*.txt"), true)
	if !p.Observed("a.o", false, false) || p.Observed("a.txt", false, false) {
		t.Fatal("parsed file not applied")
	}
	if p := r.Policy([]byte("*.md"), true); p.Observed("a.md", false, false) || !p.Observed("a.txt", false, false) {
		t.Fatal("changed file not re-parsed")
	}
	var none *Rules
	if !none.Policy(nil, false).Observed("a.o", false, false) {
		t.Fatal("nil rules ignored a path")
	}
}

func TestPolicyKeepsHeldPathsAndTheIgnoreFile(t *testing.T) {
	r, err := New("config/rules", []string{"*", "!keep"})
	if err != nil {
		t.Fatal(err)
	}
	p := r.Policy(nil, false)
	nodes := map[string]bool{
		"config": true, "config/rules": false, "config/other": false,
		"held": true, "held/child": false, "held/child2": false,
		"loose": true, "loose/keep": false, "keep": false,
	}
	kept := p.Keep(nodes, func(name string) bool { return name == "held" || name == "held/child" })
	want := map[string]bool{"config": true, "config/rules": true, "held": true, "held/child": true, "keep": true}
	if len(kept) != len(want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
	for name := range want {
		if _, ok := kept[name]; !ok {
			t.Fatalf("kept %v, want %v", kept, want)
		}
	}
}
