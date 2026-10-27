package ignore

import (
	"strings"
	"testing"
)

// Cases ported from moby/patternmatcher's TestMatches (v0.6.1,
// Apache-2.0), the matcher BuildKit and the Docker CLI use.
var patternmatcherCases = []struct {
	pattern, text string
	pass          bool
}{
	{"**", "file", true}, {"**", "file/", true}, {"**/", "file", true}, {"**/", "file/", true},
	{"**", "/", true}, {"**/", "/", true}, {"**", "dir/file", true}, {"**/", "dir/file", true},
	{"**", "dir/file/", true}, {"**/", "dir/file/", true}, {"**/**", "dir/file", true}, {"**/**", "dir/file/", true},
	{"dir/**", "dir/file", true}, {"dir/**", "dir/file/", true}, {"dir/**", "dir/dir2/file", true},
	{"dir/**", "dir/dir2/file/", true}, {"**/dir", "dir", true}, {"**/dir", "dir/file", true},
	{"**/dir2/*", "dir/dir2/file", true}, {"**/dir2/*", "dir/dir2/file/", true},
	{"**/dir2/**", "dir/dir2/dir3/file", true}, {"**/dir2/**", "dir/dir2/dir3/file/", true},
	{"**file", "file", true}, {"**file", "dir/file", true}, {"**/file", "dir/file", true},
	{"**file", "dir/dir/file", true}, {"**/file", "dir/dir/file", true}, {"**/file*", "dir/dir/file", true},
	{"**/file*", "dir/dir/file.txt", true}, {"**/file*txt", "dir/dir/file.txt", true},
	{"**/file*.txt", "dir/dir/file.txt", true}, {"**/file*.txt*", "dir/dir/file.txt", true},
	{"**/**/*.txt", "dir/dir/file.txt", true}, {"**/**/*.txt2", "dir/dir/file.txt", false},
	{"**/*.txt", "file.txt", true}, {"**/**/*.txt", "file.txt", true}, {"a**/*.txt", "a/file.txt", true},
	{"a**/*.txt", "a/dir/file.txt", true}, {"a**/*.txt", "a/dir/dir/file.txt", true},
	{"a/*.txt", "a/dir/file.txt", false}, {"a/*.txt", "a/file.txt", true}, {"a/*.txt**", "a/file.txt", true},
	{"a[b-d]e", "ae", false}, {"a[b-d]e", "ace", true}, {"a[b-d]e", "aae", false}, {"a[^b-d]e", "aze", true},
	{".*", ".foo", true}, {".*", "foo", false}, {"abc.def", "abcdef", false}, {"abc.def", "abc.def", true},
	{"abc.def", "abcZdef", false}, {"abc?def", "abcZdef", true}, {"abc?def", "abcdef", false},
	{"a\\\\", "a\\", true}, {"**/foo/bar", "foo/bar", true}, {"**/foo/bar", "dir/foo/bar", true},
	{"**/foo/bar", "dir/dir2/foo/bar", true}, {"abc/**", "abc", false}, {"abc/**", "abc/def", true},
	{"abc/**", "abc/def/ghi", true}, {"**/.foo", ".foo", true}, {"**/.foo", "bar.foo", false},
	{"a(b)c/def", "a(b)c/def", true}, {"a(b)c/def", "a(b)c/xyz", false}, {"a.|)$(}+{bc", "a.|)$(}+{bc", true},
	{"dist/proxy.py-2.4.0rc3.dev36+g08acad9-py3-none-any.whl", "dist/proxy.py-2.4.0rc3.dev36+g08acad9-py3-none-any.whl", true},
	{"dist/*.whl", "dist/proxy.py-2.4.0rc3.dev36+g08acad9-py3-none-any.whl", true},
	{"a\\*b", "a*b", true},
}

func TestPatternmatcherCases(t *testing.T) {
	for _, c := range patternmatcherCases {
		m, err := New([]string{c.pattern})
		if err != nil {
			t.Fatalf("%q: %v", c.pattern, err)
		}
		if got := m.Match(c.text); got != c.pass {
			t.Errorf("pattern %q text %q = %v, want %v", c.pattern, c.text, got, c.pass)
		}
	}
	multi := []struct {
		patterns []string
		text     string
		pass     bool
	}{
		{[]string{"**", "!util/docker/web"}, "util/docker/web/foo", false},
		{[]string{"**", "!util/docker/web", "util/docker/web/foo"}, "util/docker/web/foo", true},
		{[]string{"**", "!dist/*.whl"}, "dist/proxy.py-2.4.0rc3.dev36+g08acad9-py3-none-any.whl", false},
	}
	for _, c := range multi {
		m, err := New(c.patterns)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Match(c.text); got != c.pass {
			t.Errorf("%q %q = %v, want %v", c.patterns, c.text, got, c.pass)
		}
	}
}

// The examples of containerignore(5).
func TestContainerignoreExamples(t *testing.T) {
	pats, err := ReadAll(strings.NewReader("# comment\n*/temp*\n*/*/temp*\ntemp?\n"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(pats)
	if err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]bool{
		"somedir/temporary.txt": true, "somedir/temp": true, "somedir/subdir/temporary.txt": true,
		"tempa": true, "tempb": true, "temp": false, "temporary.txt": false, "# comment": false,
		"somedir/temp/inner": true,
	} {
		if got := m.Match(f); got != want {
			t.Errorf("%s = %v, want %v", f, got, want)
		}
	}
	check := func(file string, lines string, cases map[string]bool) {
		t.Helper()
		pats, err := ReadAll(strings.NewReader(lines))
		if err != nil {
			t.Fatal(err)
		}
		m, err := New(pats)
		if err != nil {
			t.Fatal(err)
		}
		for f, want := range cases {
			if got := m.Match(f); got != want {
				t.Errorf("%s: %s = %v, want %v", file, f, got, want)
			}
		}
	}
	check("exception", "*.md\n!README.md\n", map[string]bool{"a.md": true, "README.md": false})
	check("order1", "*.md\n!README*.md\nREADME-secret.md\n", map[string]bool{"README-secret.md": true, "README-x.md": false, "x.md": true})
	check("order2", "*.md\nREADME-secret.md\n!README*.md\n", map[string]bool{"README-secret.md": false})
	check("include-only", "*\n!src\n", map[string]bool{"src/main.go": false, "other": true})
	check("absolute", "/foo/bar\n", map[string]bool{"foo/bar": true, "x/foo/bar": false})
	check("dotdot", "a/../b\n", map[string]bool{"b": true, "a": false})
	check("bom", "\xEF\xBB\xBFsecret\n", map[string]bool{"secret": true})
}

func TestErrorsAndBounds(t *testing.T) {
	if _, err := New([]string{"!"}); err == nil {
		t.Fatal(`"!" accepted`)
	}
	if _, err := New([]string{"a["}); err == nil {
		t.Fatal("bad pattern accepted")
	}
	if _, err := ReadAll(strings.NewReader(strings.Repeat("a\n", MaxPatterns+1))); err == nil {
		t.Fatal("too many patterns accepted")
	}
	if _, err := ReadAll(strings.NewReader(strings.Repeat("a", MaxFileBytes+1))); err == nil {
		t.Fatal("oversized file accepted")
	}
	var m *Matcher
	if m.Match("x") || m.Exclusions() {
		t.Fatal("nil matcher matched")
	}
	m, _ = New([]string{"**"})
	if m.Match(".") {
		t.Fatal("the context root itself was excluded")
	}
}

func FuzzMatch(f *testing.F) {
	for _, c := range patternmatcherCases {
		f.Add(c.pattern+"\n!x", c.text)
	}
	f.Fuzz(func(t *testing.T, file, p string) {
		pats, err := ReadAll(strings.NewReader(file))
		if err != nil {
			return
		}
		m, err := New(pats)
		if err != nil {
			return
		}
		a := m.Match(p)
		if a != m.Match(p) {
			t.Fatal("not deterministic")
		}
	})
}
