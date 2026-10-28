package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
	enginecli "github.com/go-wasmcontainers/engine/cli"

	"github.com/go-wasmcontainers/build/internal/enginetest"
)

// TestWctrBuild wires the command into the engine's real wctr command tree
// through its Extra hook — the engine's cli.Engine, whose HTTP() base
// already carries the API version — then builds an image with `wctr
// build` and runs it with `wctr run`, all against a real in-process
// engine.
func TestWctrBuild(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "tool.wasm")
	b := exec.Command("go", "build", "-o", tool, "./testdata/wasm/tool")
	b.Dir = ".."
	b.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if out, err := b.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(`FROM scratch AS tools
COPY tool.wasm /bin/tool.wasm
RUN ["/bin/tool.wasm", "write", "/out/msg", "built-through-wctr"]
FROM scratch
COPY --from=tools /bin/tool.wasm /bin/
COPY --from=tools /out/msg /msg
ENTRYPOINT ["/bin/tool.wasm", "cat", "/msg"]
`), 0o644)
	e, err := enginetest.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, base := e.API().HTTP(); !strings.HasSuffix(base, "/v"+api.APIVersion) {
		t.Fatalf("the engine's client base %q no longer carries the version: this test no longer covers the doubled-version case", base)
	}
	wctr := func(args ...string) string {
		t.Helper()
		root := enginecli.RootCmd(enginecli.Options{
			Client: func(string) (*api.Client, error) { return e.API(), nil },
			Extra:  []func(enginecli.Engine) *cobra.Command{func(en enginecli.Engine) *cobra.Command { return ForEngine(en) }},
		})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("wctr %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	if out := wctr("build", "-t", "via-wctr:1", dir); !strings.Contains(out, "Successfully tagged via-wctr:1") {
		t.Fatal(out)
	}
	if out := wctr("run", "--rm", "via-wctr:1"); out != "built-through-wctr" {
		t.Fatalf("run: %q", out)
	}
}
