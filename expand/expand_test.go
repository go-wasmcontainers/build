package expand

import (
	"bufio"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBuildKitEnvVarGolden runs BuildKit's own envVarTest table (copied
// under testdata/buildkit, Apache-2.0) with the environment its test uses.
func TestBuildKitEnvVarGolden(t *testing.T) {
	f, err := os.Open("testdata/buildkit/envVarTest")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := Map{"PWD": "/home", "SHELL": "bash", "KOREAN": "한국어", "NULL": ""}
	lx := Lexer{Escape: '\\'}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			t.Fatalf("bad line %q", line)
		}
		plat, src, want := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2])
		if plat == "W" {
			continue
		}
		got, err := lx.Word(src, env)
		n++
		if want == "error" {
			if err == nil {
				t.Errorf("%q: want an error, got %q", src, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", src, err)
			continue
		}
		if got != want {
			t.Errorf("%q = %q, want %q", src, got, want)
		}
	}
	if n < 200 {
		t.Fatalf("only %d cases ran", n)
	}
}

// TestBuildKitWordsGolden runs BuildKit's wordsTest table (normal mode).
func TestBuildKitWordsGolden(t *testing.T) {
	f, err := os.Open("testdata/buildkit/wordsTest")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env := Map{}
	lx := Lexer{}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "ENV "); ok {
			k, v, _ := strings.Cut(strings.TrimLeft(rest, " "), "=")
			env[k] = v
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 2 {
			t.Fatalf("bad line %q", line)
		}
		src := strings.TrimSpace(parts[0])
		want := strings.Split(strings.TrimLeft(parts[1], " "), ",")
		got, err := lx.Words(src, env)
		n++
		if len(want) == 1 && want[0] == "error" {
			if err == nil {
				t.Errorf("%q: want an error, got %q", src, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", src, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%q = %q, want %q", src, got, want)
		}
	}
	if n < 20 {
		t.Fatalf("only %d cases ran", n)
	}
}

// The reference's documented examples ("Environment replacement").
func TestReferenceExamples(t *testing.T) {
	env := Map{"str": "foobarbaz", "string": "foobarbaz", "FOO": "/bar", "R": "$1"}
	cases := map[string]string{
		`${str#f*b}`:          "arbaz",
		`${str##f*b}`:         "az",
		`${string%b*}`:        "foobar",
		`${string%%b*}`:       "foo",
		`${string/ba/fo}`:     "fooforbaz",
		`${string//ba/fo}`:    "fooforfoz",
		`${FOO}`:              "/bar",
		`$FOO`:                "/bar",
		`\$FOO`:               "$FOO",
		`${unset:-word}`:      "word",
		`${FOO:+word}`:        "word",
		`${unset+word}`:       "",
		`${FOO-x}`:            "/bar",
		`${string/b?/\*}`:     `foo\*rbaz`,
		`${string//\?/x}`:     "foobarbaz",
		`${string/a/$}`:       "foob$rbaz",
		`${string//a/$R}`:     "foob$1rb$1z",
		`$DIRNAME/x`:          "/x",
		`"${FOO}" '${FOO}'`:   "/bar ${FOO}",
		`${FOO:-${unset:-y}}`: "/bar",
	}
	for in, want := range cases {
		got, err := Lexer{}.Word(in, env)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestBacktickEscape(t *testing.T) {
	got, err := Lexer{Escape: '`'}.Word("c:\\dir `$X", Map{"X": "y"})
	if err != nil || got != `c:\dir $X` {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A value is data: its "$" and quotes are never re-scanned.
func TestNoInjection(t *testing.T) {
	env := Map{"A": `$B "x" ${C:-boom} $(id) ` + "`id`", "B": "secret"}
	got, err := Lexer{}.Word("$A", env)
	if err != nil || got != env["A"] {
		t.Fatalf("got %q, %v", got, err)
	}
	ws, err := Lexer{}.Words("$A", env)
	if err != nil || strings.Contains(strings.Join(ws, " "), "secret") {
		t.Fatalf("words %q, %v", ws, err)
	}
}

func TestBounds(t *testing.T) {
	big := strings.Repeat("x", 600<<10)
	env := Map{"A": big}
	if _, err := (Lexer{}).Word("$A$A", env); !errors.Is(err, ErrTooLong) {
		t.Fatalf("want ErrTooLong, got %v", err)
	}
	deep := strings.Repeat("${a:-", 40) + "x" + strings.Repeat("}", 40)
	if _, err := (Lexer{}).Word(deep, nil); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("want a nesting error, got %v", err)
	}
	if _, err := (Lexer{}).Word(strings.Repeat("${a:-", 20)+"x"+strings.Repeat("}", 20), nil); err != nil {
		t.Fatal(err)
	}
}

func FuzzWord(f *testing.F) {
	for _, s := range []string{"hello", `he${XXX:-${PWD:-yyy}}xx`, `"a\"b"`, `'x'`, `${a#*}`, `${a/b/c}`, `${a//\*/x}`, "<<EOF", `\`, `${`, `$1`, "안녕$KOREAN"} {
		f.Add(s, "`")
		f.Add(s, `\`)
	}
	env := Map{"PWD": "/home", "KOREAN": "한국어", "a": "a*b?c"}
	f.Fuzz(func(t *testing.T, s, esc string) {
		lx := Lexer{Escape: '\\'}
		if esc == "`" {
			lx.Escape = '`'
		}
		w, err := lx.Word(s, env)
		if err == nil && !utf8.ValidString(s) {
			_ = w
		}
		if err == nil && len(w) > MaxOutput {
			t.Fatalf("output %d bytes", len(w))
		}
		ws, err2 := lx.Words(s, env)
		if (err == nil) != (err2 == nil) {
			t.Fatalf("Word err %v, Words err %v", err, err2)
		}
		_ = ws
	})
}
