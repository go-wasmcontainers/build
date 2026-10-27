package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootCmd(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM scratch\n"), 0o644)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cmd := RootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-H", "unix://" + filepath.Join(dir, "no.sock"), dir})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "cannot connect to the engine") {
		t.Fatalf("got %v", err)
	}
	code := -1
	osExit = func(c int) { code = c }
	defer func() { osExit = os.Exit }()
	os.Args = []string{"wctr-build", "--progress", "x", dir}
	main()
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
}
