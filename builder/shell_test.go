package builder_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-wasmcontainers/build/builder"
	"github.com/go-wasmcontainers/build/internal/enginetest"
)

var (
	shellOnce   sync.Once
	shellLayout []byte
	shellErr    error
)

// shellImageTar builds the go-wasmcontainers/shell base image (/bin/sh,
// coreutils) as an OCI layout tar from a checkout named by
// $WCTR_SHELL_SRC, the way its image workflow does. Without the variable
// the test is skipped — unless $WCTR_REQUIRE_SHELL is set (CI), where a
// skip would read as a pass.
func shellImageTar(t *testing.T) []byte {
	src := os.Getenv("WCTR_SHELL_SRC")
	if src == "" {
		if os.Getenv("WCTR_REQUIRE_SHELL") != "" {
			t.Fatal("WCTR_REQUIRE_SHELL is set but WCTR_SHELL_SRC is not")
		}
		t.Skip("set WCTR_SHELL_SRC to a go-wasmcontainers/shell checkout to run the shell-form RUN tests")
	}
	shellOnce.Do(func() {
		dir, err := os.MkdirTemp("", "wctr-shell-image")
		if err != nil {
			shellErr = err
			return
		}
		defer os.RemoveAll(dir)
		wasm := append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
		for _, c := range []struct {
			env  []string
			args []string
		}{
			{wasm, []string{"build", "-trimpath", "-o", filepath.Join(dir, "sh.wasm"), "./cmd/sh"}},
			{wasm, []string{"build", "-trimpath", "-o", filepath.Join(dir, "coreutils.wasm"), "github.com/wasmdesk/coreutils/cmd/multicall"}},
			{os.Environ(), []string{"run", "./cmd/mkimage", "--sh", filepath.Join(dir, "sh.wasm"), "--coreutils", filepath.Join(dir, "coreutils.wasm"),
				"--version", "0.0.0-test", "--layout", filepath.Join(dir, "layout")}},
		} {
			cmd := exec.Command("go", c.args...)
			cmd.Dir, cmd.Env = src, append(c.env, "GOWORK=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				shellErr = &exec.Error{Name: strings.Join(c.args, " ") + ": " + string(out), Err: err}
				return
			}
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		root := filepath.Join(dir, "layout")
		shellErr = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == root {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if d.IsDir() {
				return tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel) + "/", Typeflag: tar.TypeDir, Mode: 0o755})
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))}); err != nil {
				return err
			}
			_, err = tw.Write(b)
			return err
		})
		tw.Close()
		shellLayout = buf.Bytes()
	})
	if shellErr != nil {
		t.Fatal(shellErr)
	}
	return shellLayout
}

// loadShell loads the base image into e as shell:latest.
func loadShell(t *testing.T, e *enginetest.Engine) {
	ctx := context.Background()
	d := e.Driver()
	ref, err := d.LoadImage(ctx, bytes.NewReader(shellImageTar(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.TagImage(ctx, strings.TrimPrefix(ref, "docker.io/library/"), "shell", "latest"); err != nil {
		t.Fatalf("tag %s: %v", ref, err)
	}
}

// TestShellFormRUN: on the shell base image, shell-form RUN runs /bin/sh -c
// with pipes and redirections through the engine's WASIX process subset;
// exec-form RUN still runs a program directly.
func TestShellFormRUN(t *testing.T) {
	e := newEngine(t)
	loadShell(t, e)
	d := e.Driver()
	df := `FROM shell
RUN echo hi > /x && cat /x | wc -c > /n
RUN ["/bin/cat", "/x"]
ENV WHO=builder
RUN test "$(cat /x)" = hi && echo "made by $WHO" > /who
CMD cat /n /who
`
	var out bytes.Buffer
	res, err := builder.Build(context.Background(), builder.Options{Containerfile: []byte(df), Engine: d, Out: &out})
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "hi\n") {
		t.Fatalf("exec-form RUN output missing:\n%s", out.String())
	}
	got, code := runImage(t, d, res.ImageID)
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if code != 0 || len(lines) != 2 || strings.TrimSpace(lines[0]) != "3" || lines[1] != "made by builder" {
		t.Fatalf("run: %q (%d)", got, code)
	}
	img, _ := d.InspectImage(context.Background(), res.ImageID)
	if strings.Join(img.Config.Cmd, "|") != "/bin/sh|-c|cat /n /who" {
		t.Fatalf("CMD %q", img.Config.Cmd)
	}
	// A failing shell command stops the build with its status.
	_, err = builder.Build(context.Background(), builder.Options{Containerfile: []byte("FROM shell\nRUN false\n"), Engine: d})
	if err == nil || !strings.Contains(err.Error(), "exit code: 1") {
		t.Fatalf("RUN false: %v", err)
	}
	// A command the image does not have: the shell's 127, not the
	// missing-shell message.
	_, err = builder.Build(context.Background(), builder.Options{Containerfile: []byte("FROM shell\nRUN no-such-program\n"), Engine: d})
	if err == nil || !strings.Contains(err.Error(), "exit code: 127") || strings.Contains(err.Error(), "needs the shell") {
		t.Fatalf("missing program: %v", err)
	}
}
