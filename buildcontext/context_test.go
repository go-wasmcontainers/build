package buildcontext

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/build/ignore"
)

type ent struct {
	name, body, link string
	typ              byte
}

func tarOf(t testing.TB, es ...ent) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typ, Linkname: e.link}
		if e.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	return buf.Bytes()
}

func TestFromTarRefusesEscapes(t *testing.T) {
	if _, err := CleanName("a\x00b", DefaultLimits); !errors.Is(err, ErrEscape) {
		t.Errorf("NUL: %v", err)
	}
	for _, name := range []string{"/etc/passwd", "../x", "a/../../x"} {
		_, err := FromTar(bytes.NewReader(tarOf(t, ent{name: name, body: "x"})), DefaultLimits, "")
		if !errors.Is(err, ErrEscape) {
			t.Errorf("%q: %v", name, err)
		}
	}
	c, err := FromTar(bytes.NewReader(tarOf(t, ent{name: "./a/./b/../c", body: "x"})), DefaultLimits, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("a/c"); !ok {
		t.Fatal(c.Paths())
	}
	if e, ok := c.Get("a"); !ok || !e.Mode.IsDir() {
		t.Fatal("implicit parent missing")
	}
}

func TestLimits(t *testing.T) {
	lim := Limits{MaxBytes: 10, MaxFiles: 3, MaxPathLen: 20, MaxDepth: 3}
	if _, err := FromTar(bytes.NewReader(tarOf(t, ent{name: "a", body: strings.Repeat("x", 11)})), lim, ""); !errors.Is(err, ErrLimit) {
		t.Fatalf("bytes: %v", err)
	}
	if _, err := FromTar(bytes.NewReader(tarOf(t, ent{name: "a"}, ent{name: "b"}, ent{name: "c"})), lim, ""); !errors.Is(err, ErrLimit) {
		t.Fatalf("files: %v", err)
	}
	if _, err := FromTar(bytes.NewReader(tarOf(t, ent{name: strings.Repeat("x", 21)})), lim, ""); !errors.Is(err, ErrLimit) {
		t.Fatalf("name: %v", err)
	}
	if _, err := FromTar(bytes.NewReader(tarOf(t, ent{name: "a/b/c/d"})), lim, ""); !errors.Is(err, ErrLimit) {
		t.Fatalf("depth: %v", err)
	}
	// Truncated stream.
	b := tarOf(t, ent{name: "a", body: "0123456789"})
	if _, err := FromTar(bytes.NewReader(b[:520]), DefaultLimits, ""); err == nil {
		t.Fatal("truncated tar accepted")
	}
}

func TestLinksAndResolve(t *testing.T) {
	c, err := FromTar(bytes.NewReader(tarOf(t,
		ent{name: "dir/f", body: "F"},
		ent{name: "hard", typ: tar.TypeLink, link: "dir/f"},
		ent{name: "l1", typ: tar.TypeSymlink, link: "dir"},
		ent{name: "dir/l2", typ: tar.TypeSymlink, link: "../l1/f"},
		ent{name: "abs", typ: tar.TypeSymlink, link: "/etc"},
		ent{name: "up", typ: tar.TypeSymlink, link: "../.."},
		ent{name: "loop", typ: tar.TypeSymlink, link: "loop"},
		ent{name: "dev", typ: tar.TypeChar},
	)), DefaultLimits, "pre")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := c.Get("pre/hard"); !ok || string(e.Data) != "F" {
		t.Fatal("hard link not copied")
	}
	if _, ok := c.Get("pre/dev"); ok {
		t.Fatal("device kept")
	}
	if r, err := c.Resolve("/pre/l1/l2", true); err != nil || r != "pre/dir/f" {
		t.Fatalf("resolve: %q %v", r, err)
	}
	if r, err := c.Resolve("pre/l1", false); err != nil || r != "pre/l1" {
		t.Fatalf("no-follow: %q %v", r, err)
	}
	for _, p := range []string{"pre/abs/passwd", "pre/up/x"} {
		if _, err := c.Resolve(p, true); !errors.Is(err, ErrEscape) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := c.Resolve("pre/loop", true); err == nil {
		t.Fatal("loop resolved")
	}
	if _, err := FromTar(bytes.NewReader(tarOf(t, ent{name: "h", typ: tar.TypeLink, link: "missing"})), DefaultLimits, ""); err == nil {
		t.Fatal("dangling hard link accepted")
	}
	g, _ := c.Glob("pre/dir/*")
	if strings.Join(g, ",") != "pre/dir/f,pre/dir/l2" {
		t.Fatalf("glob %v", g)
	}
	if _, err := c.Glob("["); err == nil {
		t.Fatal("bad glob accepted")
	}
}

func TestExclude(t *testing.T) {
	c, _ := FromTar(bytes.NewReader(tarOf(t, ent{name: "a.md"}, ent{name: "README.md"}, ent{name: "docs/x.md"}, ent{name: "keep/y"})), DefaultLimits, "")
	pats, _ := ignore.ReadAll(strings.NewReader("*.md\n!README.md\ndocs\n!docs/x.md\n"))
	m, _ := ignore.New(pats)
	c.Exclude(m)
	if got := strings.Join(c.Paths(), ","); got != "README.md,docs,docs/x.md,keep,keep/y" {
		t.Fatal(got)
	}
}

func TestPackStaysInside(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.tmp"), []byte("t"), 0o644)
	os.WriteFile(filepath.Join(dir, ".containerignore"), []byte("*.tmp\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("src\n"), 0o644) // not used: .containerignore wins
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")); err != nil {
		t.Skip("no symlinks here")
	}
	var buf bytes.Buffer
	st, err := Pack(dir, &buf, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	c, err := FromTar(&buf, DefaultLimits, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get("notes.tmp"); ok {
		t.Fatal("ignored file sent")
	}
	if _, ok := c.Get("src/main.go"); !ok {
		t.Fatal("src missing")
	}
	e, ok := c.Get("link")
	if !ok || e.Link == "" || len(e.Data) != 0 {
		t.Fatal("a symlink was followed")
	}
	if _, err := c.Resolve("link", true); !errors.Is(err, ErrEscape) {
		t.Fatalf("resolve outside: %v", err)
	}
	if st.Files == 0 || e.UID != 0 {
		t.Fatalf("stats %+v", st)
	}
	if _, err := Pack(dir, &buf, Limits{MaxBytes: 1, MaxFiles: 100, MaxPathLen: 100, MaxDepth: 10}); !errors.Is(err, ErrLimit) {
		t.Fatalf("pack limit: %v", err)
	}
}

func FuzzFromTar(f *testing.F) {
	f.Add(tarOf(f, ent{name: "a/b", body: "x"}, ent{name: "l", typ: tar.TypeSymlink, link: "a"}))
	f.Add(tarOf(f, ent{name: "../x", body: "x"}))
	f.Add(tarOf(f, ent{name: "h", typ: tar.TypeLink, link: "a"}))
	f.Fuzz(func(t *testing.T, b []byte) {
		lim := Limits{MaxBytes: 1 << 16, MaxFiles: 256, MaxPathLen: 256, MaxDepth: 16}
		c, err := FromTar(bytes.NewReader(b), lim, "")
		if err != nil {
			return
		}
		if c.Len() > lim.MaxFiles || c.Bytes() > lim.MaxBytes {
			t.Fatalf("limits exceeded: %d files %d bytes", c.Len(), c.Bytes())
		}
		for _, p := range c.Paths() {
			if strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
				t.Fatalf("escaping path %q", p)
			}
			if r, err := c.Resolve(p, true); err == nil && (strings.HasPrefix(r, "../") || r == "..") {
				t.Fatalf("resolved outside: %q", r)
			}
		}
	})
}
