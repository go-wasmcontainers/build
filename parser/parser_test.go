package parser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildKitGolden parses every file of BuildKit's parser test corpus
// (testdata/buildkit, Apache-2.0) and compares with its expected dump.
func TestBuildKitGolden(t *testing.T) {
	dirs, err := os.ReadDir("testdata/buildkit/testfiles")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 30 {
		t.Fatalf("only %d golden cases", len(dirs))
	}
	for _, d := range dirs {
		t.Run(d.Name(), func(t *testing.T) {
			dir := filepath.Join("testdata/buildkit/testfiles", d.Name())
			f, err := os.Open(filepath.Join(dir, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			res, err := Parse(f, DefaultLimits)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(dir, "result"))
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Dump(); got != string(want) {
				t.Errorf("dump differs\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestBuildKitNegative(t *testing.T) {
	dirs, err := os.ReadDir("testdata/buildkit/testfiles-negative")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		t.Run(d.Name(), func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata/buildkit/testfiles-negative", d.Name(), "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := Parse(f, DefaultLimits); err == nil {
				t.Fatal("parsed")
			}
		})
	}
}

func parse(t *testing.T, s string) *Result {
	t.Helper()
	r, err := Parse(strings.NewReader(s), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The examples of the reference's "Format" and "Parser directives"
// sections.
func TestReferenceDirectives(t *testing.T) {
	r := parse(t, "# escape=`\n\nFROM microsoft/nanoserver\nCOPY testfile.txt c:\\\nRUN dir c:\\\n")
	if r.Escape != '`' || len(r.Nodes) != 3 || r.Nodes[1].Args[1] != `c:\` {
		t.Fatalf("got %q escape %q", r.Dump(), r.Escape)
	}
	// Without the directive the backslash continues the line.
	r = parse(t, "FROM microsoft/nanoserver\nCOPY testfile.txt c:\\\nRUN dir c:\\\n")
	if len(r.Nodes) != 2 || r.Nodes[1].Args[2] != "dir" {
		t.Fatalf("got %q", r.Dump())
	}
	// Directives after a comment, instruction, or twice.
	r = parse(t, "# About my dockerfile\n# escape=`\nFROM x\n")
	if r.Escape != '\\' {
		t.Fatal("directive after a comment was honoured")
	}
	r = parse(t, "FROM x\n# escape=`\nRUN a\n")
	if r.Escape != '\\' {
		t.Fatal("directive after an instruction was honoured")
	}
	if _, err := Parse(strings.NewReader("# escape=\\\n# escape=`\nFROM x\n"), DefaultLimits); err == nil {
		t.Fatal("duplicate directive accepted")
	}
	for _, s := range []string{"#directive=value", "# directive =value", "#\tdirective= value", "# directive = value", "#\t  dIrEcTiVe=value"} {
		_ = parse(t, strings.ReplaceAll(s, "directive", "syntax")+"\nFROM x\n")
	}
	r = parse(t, "#\t  SyNtAx = docker/dockerfile:1 \nFROM x\n")
	if r.Directives["syntax"] != "docker/dockerfile:1" {
		t.Fatalf("directives %v", r.Directives)
	}
	if _, err := Parse(strings.NewReader("# escape=x\nFROM x\n"), DefaultLimits); err == nil {
		t.Fatal("bad escape accepted")
	}
}

func TestReferenceFormat(t *testing.T) {
	r := parse(t, "# Comment\nRUN echo 'we are running some # of cool things'\n")
	if r.Nodes[0].Args[0] != "echo 'we are running some # of cool things'" {
		t.Fatal(r.Dump())
	}
	r = parse(t, "RUN echo hello \\\n# comment\nworld\n")
	if r.Nodes[0].Args[0] != "echo hello world" {
		t.Fatalf("%q", r.Nodes[0].Args[0])
	}
	r = parse(t, "        # this is a comment-line\n    RUN echo hello\nRUN echo world\n")
	if len(r.Nodes) != 2 || r.Nodes[0].Args[0] != "echo hello" {
		t.Fatal(r.Dump())
	}
	r = parse(t, "RUN echo \"\\\n     hello\\\n     world\"\n")
	if r.Nodes[0].Args[0] != `echo "     hello     world"` {
		t.Fatalf("%q", r.Nodes[0].Args[0])
	}
	r = parse(t, "RUN a \\\n\nb\n")
	if len(r.Warnings) != 1 || r.Nodes[0].Args[0] != "a b" {
		t.Fatalf("%q %v", r.Nodes[0].Args, r.Warnings)
	}
	if r.Nodes[0].StartLine != 1 || r.Nodes[0].EndLine != 3 {
		t.Fatalf("lines %d-%d", r.Nodes[0].StartLine, r.Nodes[0].EndLine)
	}
}

func TestForms(t *testing.T) {
	r := parse(t, `FROM --platform=wasip1/wasm base:1 AS build
RUN ["prog", "a b"]
RUN prog "a b"
CMD ["x"]
COPY --from=build --chown=1:1 a b /dst/
COPY ["a b", "/c d/"]
ENV A=1 B="x y"
ENV LEG a b c
ARG V=1 W
LABEL "k"="v"
ONBUILD RUN ["x"]
HEALTHCHECK --interval=5s CMD ["probe"]
HEALTHCHECK NONE
EXPOSE 80/tcp 53/udp
VOLUME ["/data"]
SHELL ["/bin/sh", "-c"]
`)
	want := []string{
		`from ["--platform=wasip1/wasm"] "base:1" "AS" "build"`,
		`run "prog" "a b"`,
		`run "prog \"a b\""`,
		`cmd "x"`,
		`copy ["--from=build" "--chown=1:1"] "a" "b" "/dst/"`,
		`copy "a b" "/c d/"`,
		`env "A" "1" "=" "B" "\"x y\"" "="`,
		`env "LEG" "a b c" ""`,
		`arg "V=1" "W"`,
		`label "\"k\"" "\"v\"" "="`,
		`onbuild (run "x")`,
		`healthcheck ["--interval=5s"] "CMD" "probe"`,
		`healthcheck "NONE"`,
		`expose "80/tcp" "53/udp"`,
		`volume "/data"`,
		`shell "/bin/sh" "-c"`,
	}
	for i, w := range want {
		if got := r.Nodes[i].Dump(); got != w {
			t.Errorf("%d: %s, want %s", i, got, w)
		}
	}
	if !r.Nodes[1].JSON || r.Nodes[2].JSON {
		t.Error("JSON form not reported")
	}
}

func TestErrors(t *testing.T) {
	for _, s := range []string{
		"FOO bar\n",
		"RUN [1, 2]\n",
		"ONBUILD ONBUILD RUN x\n",
		"ONBUILD FROM x\n",
		"ENV A\n",
		"ENV a=1 b\n",
		"RUN <<EOF\necho\nEOF\n",
		"COPY <<-EOT /x\nhi\nEOT\n",
		"",
		"# only a comment\n",
		"FROM x\xff\n",
	} {
		if _, err := Parse(strings.NewReader(s), DefaultLimits); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
	// Not heredocs.
	parse(t, "RUN echo a<<b\nRUN [\"<<EOF\"]\nRUN cat << \n")
}

func TestLimits(t *testing.T) {
	lim := Limits{MaxBytes: 100, MaxLineBytes: 50, MaxInstructions: 2}
	if _, err := Parse(strings.NewReader(strings.Repeat("#", 101)), lim); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size: %v", err)
	}
	if _, err := Parse(strings.NewReader("RUN "+strings.Repeat("x", 60)+"\n"), lim); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("line: %v", err)
	}
	if _, err := Parse(strings.NewReader("RUN a\nRUN b\nRUN c\n"), lim); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("count: %v", err)
	}
	var pe *Error
	if _, err := Parse(strings.NewReader("FROM x\nBAD y\n"), DefaultLimits); !errors.As(err, &pe) || pe.Line != 2 {
		t.Fatalf("location: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	dirs, _ := os.ReadDir("testdata/buildkit/testfiles")
	for _, d := range dirs {
		if b, err := os.ReadFile(filepath.Join("testdata/buildkit/testfiles", d.Name(), "Dockerfile")); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("# escape=`\nFROM x `\n AS y\nONBUILD COPY --from=a b c\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := Parse(strings.NewReader(string(b)), Limits{MaxBytes: 64 << 10, MaxLineBytes: 16 << 10, MaxInstructions: 256})
		if err != nil {
			return
		}
		if len(r.Nodes) == 0 || len(r.Nodes) > 256 {
			t.Fatalf("%d nodes", len(r.Nodes))
		}
		for _, n := range r.Nodes {
			if _, ok := known[n.Cmd]; !ok && n.Cmd != "onbuild" {
				t.Fatalf("unknown %q accepted", n.Cmd)
			}
			_ = n.Dump()
		}
	})
}
