package builder_test

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/build/buildcontext"
	"github.com/go-wasmcontainers/build/builder"
	"github.com/go-wasmcontainers/build/driver"
	"github.com/go-wasmcontainers/build/internal/enginetest"
)

var (
	toolOnce sync.Once
	toolWasm []byte
	toolErr  error
)

// tool compiles testdata/wasm/tool for wasip1 once.
func tool(t testing.TB) []byte {
	toolOnce.Do(func() {
		dir, err := os.MkdirTemp("", "wctr-build-tool")
		if err != nil {
			toolErr = err
			return
		}
		out := filepath.Join(dir, "tool.wasm")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/wasm/tool")
		cmd.Dir = ".."
		cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			toolErr = err
			t.Log(string(b))
			return
		}
		toolWasm, toolErr = os.ReadFile(out)
		os.RemoveAll(dir)
	})
	if toolErr != nil {
		t.Fatal(toolErr)
	}
	return toolWasm
}

func newEngine(t testing.TB) *enginetest.Engine {
	e, err := enginetest.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

// contextOf makes a build context from name → content.
func contextOf(t testing.TB, files map[string]string) *buildcontext.Context {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for n, c := range files {
		mode := int64(0o644)
		if strings.HasSuffix(n, ".wasm") {
			mode = 0o755
		}
		if target, ok := strings.CutPrefix(c, "->"); ok {
			tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: n, Mode: mode, Size: int64(len(c)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0)})
		tw.Write([]byte(c))
	}
	tw.Close()
	bc, err := buildcontext.FromTar(&buf, buildcontext.DefaultLimits, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := bc.IgnoreMatcher()
	if err != nil {
		t.Fatal(err)
	}
	bc.Exclude(m)
	return bc
}

// runImage runs an image with its own command (or cmd) and returns the
// output and status.
func runImage(t testing.TB, e driver.Engine, image string, cmd ...string) (string, int) {
	t.Helper()
	ctx := context.Background()
	id, err := e.CreateContainer(ctx, driver.RunSpec{Image: image, Entrypoint: cmd})
	if err != nil {
		t.Fatal(err)
	}
	defer e.RemoveContainer(ctx, id)
	var out bytes.Buffer
	code, err := e.Run(ctx, id, &out, &out)
	if err != nil {
		t.Fatalf("run %s: %v (%s)", image, err, out.String())
	}
	return out.String(), code
}

const multiStage = `# syntax=docker/dockerfile:1
ARG BASE=scratch
FROM ${BASE} AS tools
COPY tool.wasm /bin/tool.wasm
ARG GREETING=hi
ENV STAGE=tools
WORKDIR /work
RUN ["/bin/tool.wasm", "write", "/out/msg", "made-in-run"]
RUN ["/bin/tool.wasm", "envfile", "GREETING", "/out/greeting"]
RUN ["/bin/tool.wasm", "envfile", "STAGE", "/out/stage"]

FROM scratch AS final
COPY --from=tools /bin/tool.wasm /bin/
COPY --from=tools /out/ /data/
COPY data/ /data/
LABEL org.example.k="v w"
EXPOSE 8080 53/udp
ENTRYPOINT ["/bin/tool.wasm", "cat"]
CMD ["/data/msg"]
`

func TestMultiStageBuildAndRun(t *testing.T) {
	e := newEngine(t)
	d := e.Driver()
	files := map[string]string{
		"tool.wasm": string(tool(t)), "data/hello.txt": "hello", "data/sub/x": "x",
		".containerignore": "secret*\n", "secret.txt": "s3cret",
	}
	ctx := context.Background()
	cache := builder.NewMemoryCache(0)
	var out bytes.Buffer
	res, err := builder.Build(ctx, builder.Options{
		Containerfile: []byte(multiStage), Context: contextOf(t, files), Engine: d, Cache: cache,
		Out: &out, Tags: []string{"lab/app:v1"}, BuildArgs: map[string]string{"GREETING": "hello-arg"},
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got, code := runImage(t, d, "lab/app:v1"); got != "made-in-run" || code != 0 {
		t.Fatalf("run: %q %d\n%s", got, code, out.String())
	}
	if got, _ := runImage(t, d, "lab/app:v1", "/bin/tool.wasm", "cat", "/data/greeting", "/data/stage", "/data/hello.txt", "/data/sub/x"); got != "hello-argtoolshellox" {
		t.Fatalf("files: %q", got)
	}
	img, err := d.InspectImage(ctx, res.ImageID)
	if err != nil {
		t.Fatal(err)
	}
	c := img.Config
	if c.Labels["org.example.k"] != "v w" || len(c.ExposedPorts) != 2 || strings.Join(c.Entrypoint, " ") != "/bin/tool.wasm cat" || strings.Join(c.Cmd, " ") != "/data/msg" {
		t.Fatalf("config %+v", c)
	}
	for _, env := range c.Env {
		if strings.HasPrefix(env, "STAGE=") || strings.HasPrefix(env, "GREETING=") {
			t.Fatalf("the tools stage's ENV/ARG leaked into the final image: %v", c.Env)
		}
	}
	if img.Layers != 3 {
		t.Fatalf("final image has %d layers, want 3 (three COPY)", img.Layers)
	}
	if !strings.Contains(out.String(), "Successfully tagged lab/app:v1") || !strings.Contains(out.String(), "syntax directive") {
		t.Fatalf("progress:\n%s", out.String())
	}

	// The same build again: every step comes from the cache.
	out.Reset()
	steps := e.Calls["commit"]
	res2, err := builder.Build(ctx, builder.Options{
		Containerfile: []byte(multiStage), Context: contextOf(t, files), Engine: d, Cache: cache,
		Out: &out, BuildArgs: map[string]string{"GREETING": "hello-arg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.ImageID != res.ImageID || e.Calls["commit"] != steps || res2.Cached == 0 {
		t.Fatalf("rebuild: %s vs %s, %d new commits, %d cached\n%s", res2.ImageID, res.ImageID, e.Calls["commit"]-steps, res2.Cached, out.String())
	}

	// A changed build-arg misses from the first RUN that sees it on.
	out.Reset()
	res3, err := builder.Build(ctx, builder.Options{
		Containerfile: []byte(multiStage), Context: contextOf(t, files), Engine: d, Cache: cache,
		Out: &out, BuildArgs: map[string]string{"GREETING": "other"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// COPY, ENV and WORKDIR hit; every RUN after "ARG GREETING" misses, as
	// the reference says (an ARG is in the environment of every later RUN).
	if res3.ImageID == res.ImageID || res3.Cached != 3 {
		t.Fatalf("changed arg: same image or too few hits (%d)\n%s", res3.Cached, out.String())
	}
	if got, _ := runImage(t, d, res3.ImageID, "/bin/tool.wasm", "cat", "/data/greeting"); got != "other" {
		t.Fatalf("greeting %q", got)
	}

	// --target stops at a stage.
	res4, err := builder.Build(ctx, builder.Options{Containerfile: []byte(multiStage), Context: contextOf(t, files), Engine: d, Cache: cache, Target: "tools"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := runImage(t, d, res4.ImageID, "/bin/tool.wasm", "pwd"); got != "/work\n" {
		t.Fatalf("pwd in the tools stage: %q", got)
	}
}

func TestIgnoredAndMissingSources(t *testing.T) {
	e := newEngine(t)
	files := map[string]string{"tool.wasm": "x", ".dockerignore": "secret*\n", "secret.txt": "s"}
	for df, want := range map[string]string{
		"FROM scratch\nCOPY secret.txt /\n":                        "excluded by the ignore file",
		"FROM scratch\nCOPY nope /\n":                              "not found",
		"FROM scratch\nCOPY *.none /\n":                            "no source files",
		"FROM scratch\nCOPY tool.wasm .dockerignore /x\n":          "must be a directory",
		"FROM scratch\nADD https://example.com/x /\n":              "not supported yet",
		"FROM --platform=linux/amd64 scratch\n":                    "only wasip1/wasm",
		"FROM scratch\nRUN --mount=type=cache,target=/x [\"a\"]\n": "not supported yet",
		"FROM scratch\nCOPY --chown=bob tool.wasm /\n":             "numeric",
		"FROM scratch\nCOPY --from=0 /x /y\n":                      "not a previous stage",
		"RUN x\n":                                                  "must begin with FROM",
		"FROM scratch\nSTOPSIGNAL $(kill)\n":                       "invalid signal",
	} {
		ignored := strings.Contains(df, "secret")
		_, err := builder.Build(context.Background(), builder.Options{Containerfile: []byte(df), Context: contextOf(t, files), Engine: e.Driver(), Ignored: ignored})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", df, err, want)
		}
	}
}

// ENV values that double at each step stop at the bound instead of
// exhausting memory.
func TestVariableGrowthIsBounded(t *testing.T) {
	e := newEngine(t)
	df := "FROM scratch\nENV A=" + strings.Repeat("x", 200<<10) + "\nENV A=$A$A\nENV A=$A$A\nENV B=$A$A\nENV C=$B$B\nENV D=$C$C\n"
	_, err := builder.Build(context.Background(), builder.Options{Containerfile: []byte(df), Engine: e.Driver()})
	if err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("got %v", err)
	}
}

func TestShellForm(t *testing.T) {
	e := newEngine(t)
	d := e.Driver()
	tw := string(tool(t))
	ctx := context.Background()
	// No shell in the image: the precise error.
	_, err := builder.Build(ctx, builder.Options{Containerfile: []byte("FROM scratch\nCOPY tool.wasm /bin/tool.wasm\nRUN tool write /x y\n"), Context: contextOf(t, map[string]string{"tool.wasm": tw}), Engine: d})
	if err == nil || !strings.Contains(err.Error(), "needs the shell \"/bin/sh\"") {
		t.Fatalf("got %v", err)
	}
	// A builtin-only shell at /bin/sh runs shell-form RUN; SHELL changes it.
	df := `FROM scratch
COPY tool.wasm /bin/sh
RUN write /made-by-sh yes
COPY tool.wasm /bin/alt.wasm
SHELL ["/bin/alt.wasm", "-c"]
RUN write /made-by-alt yes
CMD cat /made-by-sh /made-by-alt
`
	res, err := builder.Build(ctx, builder.Options{Containerfile: []byte(df), Context: contextOf(t, map[string]string{"tool.wasm": tw}), Engine: d})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := runImage(t, d, res.ImageID); got != "yesyes" {
		t.Fatalf("got %q", got)
	}
	img, _ := d.InspectImage(ctx, res.ImageID)
	if strings.Join(img.Config.Cmd, "|") != "/bin/alt.wasm|-c|cat /made-by-sh /made-by-alt" {
		t.Fatalf("CMD in shell form: %q", img.Config.Cmd)
	}
	// A failing step stops the build with its status.
	_, err = builder.Build(ctx, builder.Options{Containerfile: []byte("FROM scratch\nCOPY tool.wasm /t.wasm\nRUN [\"/t.wasm\", \"exit\", \"3\"]\n"), Context: contextOf(t, map[string]string{"tool.wasm": tw}), Engine: d})
	if err == nil || !strings.Contains(err.Error(), "exit code: 3") {
		t.Fatalf("got %v", err)
	}
}

func TestOnBuildAndMetadata(t *testing.T) {
	e := newEngine(t)
	d := e.Driver()
	tw := string(tool(t))
	ctx := context.Background()
	parent := `FROM scratch
COPY tool.wasm /bin/tool.wasm
ONBUILD COPY marker /marker
ONBUILD RUN ["/bin/tool.wasm", "write", "/triggered", "yes"]
ENV A=1 B="two words"
ENV C=${A}-${B:-none}
USER 1000:1000
VOLUME ["/data"]
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=5s --retries=2 CMD ["/bin/tool.wasm", "pwd"]
WORKDIR rel
WORKDIR sub
`
	if _, err := builder.Build(ctx, builder.Options{Containerfile: []byte(parent), Context: contextOf(t, map[string]string{"tool.wasm": tw}), Engine: d, Tags: []string{"onbuild-base"}}); err != nil {
		t.Fatal(err)
	}
	pimg, err := d.InspectImage(ctx, "onbuild-base")
	if err != nil {
		t.Fatal(err)
	}
	pc := pimg.Config
	if len(pc.OnBuild) != 2 || pc.User != "1000:1000" || pc.WorkingDir != "/rel/sub" || pc.StopSignal != "SIGTERM" || pc.Healthcheck == nil || pc.Healthcheck.Retries != 2 || pc.Healthcheck.Interval != int64(5*time.Second) {
		t.Fatalf("parent config %+v", pc)
	}
	if strings.Join(pc.Env, ",") != "A=1,B=two words,C=1-two words" {
		t.Fatalf("env %q", pc.Env)
	}
	res, err := builder.Build(ctx, builder.Options{Containerfile: []byte("FROM onbuild-base\nCMD [\"/bin/tool.wasm\", \"cat\", \"/marker\", \"/triggered\"]\n"), Context: contextOf(t, map[string]string{"marker": "m:"}), Engine: d})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := runImage(t, d, res.ImageID); got != "m:yes" {
		t.Fatalf("got %q", got)
	}
	cimg, _ := d.InspectImage(ctx, res.ImageID)
	if len(cimg.Config.OnBuild) != 0 {
		t.Fatal("triggers were inherited by the child")
	}
}

func TestCOPYSemantics(t *testing.T) {
	e := newEngine(t)
	d := e.Driver()
	tw := string(tool(t))
	ctx := context.Background()
	files := map[string]string{
		"tool.wasm": tw, "a.txt": "A", "b.txt": "B", "dir/x": "X", "dir/y/z": "Z",
		"link": "->a.txt", "escape": "->../../etc/passwd",
	}
	df := `FROM scratch
COPY tool.wasm /bin/t.wasm
WORKDIR /w
COPY *.txt sub/
COPY dir /d
COPY a.txt /renamed
COPY ["a.txt", "/sp ace/"]
COPY --chmod=600 b.txt /b
COPY link /l/
COPY ../../a.txt /up
`
	res, err := builder.Build(ctx, builder.Options{Containerfile: []byte(df), Context: contextOf(t, files), Engine: d})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := runImage(t, d, res.ImageID, "/bin/t.wasm", "cat", "/w/sub/a.txt", "/w/sub/b.txt", "/d/x", "/d/y/z", "/renamed", "/sp ace/a.txt", "/b", "/l/link", "/up")
	if got != "ABXZAABAA" {
		t.Fatalf("got %q", got)
	}
	_, err = builder.Build(ctx, builder.Options{Containerfile: []byte("FROM scratch\nCOPY escape/x /\n"), Context: contextOf(t, files), Engine: d})
	if err == nil || !strings.Contains(err.Error(), "leaves the build context") {
		t.Fatalf("escape: %v", err)
	}
}

func TestADDArchive(t *testing.T) {
	var arch bytes.Buffer
	tw := tar.NewWriter(&arch)
	tw.WriteHeader(&tar.Header{Name: "in/f", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("ok"))
	tw.Close()
	e := newEngine(t)
	d := e.Driver()
	res, err := builder.Build(context.Background(), builder.Options{
		Containerfile: []byte("FROM scratch\nCOPY tool.wasm /t.wasm\nADD a.tar /x/\nADD --unpack=false a.tar /raw/\n"),
		Context:       contextOf(t, map[string]string{"tool.wasm": string(tool(t)), "a.tar": arch.String()}), Engine: d,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := runImage(t, d, res.ImageID, "/t.wasm", "cat", "/x/in/f"); got != "ok" {
		t.Fatalf("got %q", got)
	}
	if got, _ := runImage(t, d, res.ImageID, "/t.wasm", "cat", "/raw/a.tar"); got != arch.String() {
		t.Fatal("ADD --unpack=false extracted")
	}
}
