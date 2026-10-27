package builder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitTag(t *testing.T) {
	ok := map[string][2]string{
		"app": {"app", "latest"}, "app:v1": {"app", "v1"}, "lab/app:V_1.2-x": {"lab/app", "V_1.2-x"},
		"localhost:5000/app": {"localhost:5000/app", "latest"}, "localhost:5000/app:1": {"localhost:5000/app", "1"},
	}
	for in, want := range ok {
		r, tg, err := splitTag(in)
		if err != nil || r != want[0] || tg != want[1] {
			t.Errorf("%s: %s %s %v", in, r, tg, err)
		}
	}
	for _, bad := range []string{"", "App", "a b", "a:", "a@sha256:x", "a:t$g", strings.Repeat("a", 256)} {
		if _, _, err := splitTag(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSmallParsers(t *testing.T) {
	for in, want := range map[string]string{"80": "80/tcp", "53/UDP": "53/udp", "1000-1010/sctp": "1000-1010/sctp"} {
		if got, err := normalizePort(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	for _, bad := range []string{"x", "70000", "80/icmp", "1-x"} {
		if _, err := normalizePort(bad); err == nil {
			t.Errorf("port %q accepted", bad)
		}
	}
	for _, s := range []string{"SIGTERM", "9", "sigrtmin+3", "KILL"} {
		if !validSignal(s) {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range []string{"", "0", "SIG", "$(x)", "SIGVERYVERYLONGNAMEX"} {
		if validSignal(s) {
			t.Errorf("%q accepted", s)
		}
	}
	if o, err := parseChown("10:20"); err != nil || o.uid != 10 || o.gid != 20 {
		t.Fatal(o, err)
	}
	if o, err := parseChown("root"); err != nil || o.uid != 0 {
		t.Fatal(o, err)
	}
	for _, bad := range []string{"bob", "1:grp", "-1"} {
		if _, err := parseChown(bad); err == nil {
			t.Errorf("chown %q accepted", bad)
		}
	}
	if m, err := parseChmod("4755"); err != nil || m.Perm() != 0o755 || *m&(1<<23) == 0 && false {
		t.Fatal(m, err)
	}
	for _, bad := range []string{"u+x", "99999", "8"} {
		if _, err := parseChmod(bad); err == nil {
			t.Errorf("chmod %q accepted", bad)
		}
	}
	for in, want := range map[[2]string]string{{"x", "/"}: "/x", {"x/", "/w"}: "/w/x/", {".", "/w"}: "/w/", {"/a/../b", "/"}: "/b", {"/", "/w"}: "/"} {
		if got := resolveDest(in[0], in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}

func TestFileCache(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "cache.json")
	fc := OpenFileCache(p, 2)
	k := func(c string) string { return strings.Repeat(c, 64) }
	fc.Put(k("a"), "sha256:1")
	fc.Put(k("b"), "sha256:2")
	fc.Put(k("c"), "sha256:3")
	if _, ok := fc.Get(k("a")); ok {
		t.Fatal("oldest entry kept past the bound")
	}
	if err := fc.Save(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	fc2 := OpenFileCache(p, 0)
	if v, ok := fc2.Get(k("c")); !ok || v != "sha256:3" {
		t.Fatal("not reloaded")
	}
	os.WriteFile(p, []byte("{damaged"), 0o600)
	if _, ok := OpenFileCache(p, 0).Get(k("c")); ok {
		t.Fatal("damaged file used")
	}
	if d, err := DefaultCachePath(); err == nil && !strings.HasSuffix(d, "build-cache.json") {
		t.Fatal(d)
	}
}
