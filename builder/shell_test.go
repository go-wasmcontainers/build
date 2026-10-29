package builder_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/build/builder"
	"github.com/go-wasmcontainers/build/internal/enginetest"
)

// ShellImage is the go-wasmcontainers/shell base image (/bin/sh +
// coreutils, wasip1/wasm) these tests build on: v0.1.0, pinned by its
// manifest digest so the tests run against the same bytes every time.
const ShellImage = "ghcr.io/go-wasmcontainers/shell@sha256:79b185bfd505b523227396208976641e616723c3238c2a2edf18adcf15423b0e"

// loadShell pulls the base image into e (anonymously, from ghcr.io) and
// tags it shell:latest. A failed pull fails the test: skipping would read
// as a pass.
func loadShell(t *testing.T, e *enginetest.Engine) {
	t.Helper()
	ctx := context.Background()
	d := e.Driver()
	var progress bytes.Buffer
	if err := d.PullImage(ctx, ShellImage, &progress); err != nil {
		t.Fatalf("pulling the shell base image %s: %v\n%s", ShellImage, err, progress.String())
	}
	if err := d.TagImage(ctx, ShellImage, "shell", "latest"); err != nil {
		t.Fatalf("tag %s: %v", ShellImage, err)
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
	// The image's own SHELL is honoured, and FROM takes the digest reference.
	_, err = builder.Build(context.Background(), builder.Options{Containerfile: []byte("FROM " + ShellImage + "\nRUN false\n"), Engine: d})
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
