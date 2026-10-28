package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/build/builder"
	"github.com/go-wasmcontainers/build/driver"
	"github.com/go-wasmcontainers/build/internal/enginetest"
)

func run(t *testing.T, opt Options, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewCommand(opt)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestBuildCommand(t *testing.T) {
	e, err := enginetest.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	opt := Options{Engine: func(*cobra.Command) (driver.Engine, error) { return e.Driver(), nil }, Cache: builder.NewMemoryCache(0)}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nARG V\nLABEL v=$V\nCOPY a.txt /\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("b.txt\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B"), 0o644)

	t.Setenv("FROMENV", "env-value")
	out, err := run(t, opt, "", "-t", "cli/test:1", "--build-arg", "V", "--build-arg", "V=x", dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Successfully tagged cli/test:1") {
		t.Fatal(out)
	}
	img, err := e.Driver().InspectImage(context.Background(), "cli/test:1")
	if err != nil || img.Config.Labels["v"] != "x" {
		t.Fatalf("%v %+v", err, img)
	}
	// -q prints only the ID; the second run is cached.
	out, err = run(t, opt, "", "-q", "--build-arg", "V=x", dir)
	if err != nil || strings.TrimSpace(out) != img.ID {
		t.Fatalf("quiet: %q %v (want %s)", out, err, img.ID)
	}
	// Containerfile wins over Dockerfile; -f and -f - choose.
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM scratch\nLABEL which=containerfile\n"), 0o644)
	if out, err = run(t, opt, "", "-t", "which", dir); err != nil {
		t.Fatal(err, out)
	}
	if img, _ := e.Driver().InspectImage(context.Background(), "which"); img.Config.Labels["which"] != "containerfile" {
		t.Fatal("Containerfile not preferred")
	}
	if out, err = run(t, opt, "FROM scratch\nLABEL which=stdin\n", "-f", "-", "-t", "which", dir); err != nil {
		t.Fatal(err, out)
	}
	if img, _ := e.Driver().InspectImage(context.Background(), "which"); img.Config.Labels["which"] != "stdin" {
		t.Fatal("-f - not read")
	}
	// The ignored file cannot be copied.
	os.WriteFile(filepath.Join(dir, "ign"), []byte("FROM scratch\nCOPY b.txt /\n"), 0o644)
	if _, err = run(t, opt, "", "-f", filepath.Join(dir, "ign"), dir); err == nil || !strings.Contains(err.Error(), "excluded by the ignore file") {
		t.Fatalf("ignored: %v", err)
	}
	// A context tar on stdin.
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	df := "FROM scratch\nCOPY f /f\n"
	tw.WriteHeader(&tar.Header{Name: "Containerfile", Mode: 0o644, Size: int64(len(df)), Typeflag: tar.TypeReg})
	tw.Write([]byte(df))
	tw.WriteHeader(&tar.Header{Name: "f", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("f"))
	tw.Close()
	if out, err = run(t, opt, tb.String(), "-"); err != nil {
		t.Fatal(err, out)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--progress", "tty", dir}, "only plain"},
		{[]string{filepath.Join(dir, "nope")}, "unable to prepare context"},
		{[]string{filepath.Join(dir, "a.txt")}, "not a directory"},
		{[]string{"-f", filepath.Join(dir, "nope"), dir}, "no such file"},
		{[]string{"--build-arg", "=x", dir}, "KEY=VALUE"},
		{[]string{"-f", "-", "-"}, "cannot both"},
		{[]string{"--target", "nope", dir}, "could not be found"},
	} {
		if _, err := run(t, opt, "", c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
	empty := t.TempDir()
	if _, err := run(t, opt, "", empty); err == nil || !strings.Contains(err.Error(), "no Containerfile") {
		t.Fatalf("empty: %v", err)
	}
}

func TestDefaultEngine(t *testing.T) {
	t.Setenv("WCTR_HOST", "tcp://x")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM scratch\n"), 0o644)
	_, err := run(t, Options{Cache: builder.NewMemoryCache(0)}, "", dir)
	if err == nil || !strings.Contains(err.Error(), "unsupported engine host") {
		t.Fatalf("got %v", err)
	}
	t.Setenv("WCTR_HOST", "unix://"+filepath.Join(dir, "none.sock"))
	_, err = run(t, Options{Cache: builder.NewMemoryCache(0)}, "", dir)
	if err == nil || !strings.Contains(err.Error(), "cannot connect") {
		t.Fatalf("got %v", err)
	}
	if Command().Use == "" {
		t.Fatal("no use line")
	}
}
