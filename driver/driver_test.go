package driver_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/build/driver"
	"github.com/go-wasmcontainers/build/internal/enginetest"
)

// emptyLayout is an OCI layout tar of one empty wasip1/wasm image.
func emptyLayout(t *testing.T, name string) []byte {
	cfg := []byte(`{"architecture":"wasm","os":"wasip1","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	cd := enginetest.Digest(cfg)
	man := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + cd + `","size":` + itoa(len(cfg)) + `},"layers":[]}`)
	md := enginetest.Digest(man)
	ann := ""
	if name != "" {
		ann = `,"annotations":{"org.opencontainers.image.ref.name":"` + name + `"}`
	}
	idx := []byte(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + md + `","size":` + itoa(len(man)) + ann + `}]}`)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for n, b := range map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`), "index.json": idx,
		"blobs/sha256/" + cd[7:]: cfg, "blobs/sha256/" + md[7:]: man} {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	tw.Close()
	return buf.Bytes()
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestContract runs the driver against the engine and against the
// prototype its endpoints were specified from: both must answer alike.
func TestContract(t *testing.T) {
	for name, mk := range map[string]func(string) (*enginetest.Engine, error){"engine": enginetest.New, "prototype": enginetest.NewPrototype} {
		t.Run(name, func(t *testing.T) {
			e, err := mk(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			d := e.Driver()
			ctx := context.Background()
			if _, err := d.InspectImage(ctx, "nope"); !errors.Is(err, driver.ErrNotFound) {
				t.Fatalf("missing image: %v", err)
			}
			id, err := d.LoadImage(ctx, bytes.NewReader(emptyLayout(t, "")))
			if err != nil || !strings.HasPrefix(id, "sha256:") {
				t.Fatalf("load dangling: %q %v", id, err)
			}
			if named, err := d.LoadImage(ctx, bytes.NewReader(emptyLayout(t, "base:1"))); err != nil || !strings.Contains(named, "base") {
				t.Fatalf("load named: %q %v", named, err)
			}
			img, err := d.InspectImage(ctx, id)
			if err != nil || img.OS != "wasip1" || img.Layers != 0 {
				t.Fatalf("inspect dangling: %+v %v", img, err)
			}
			c, err := d.CreateContainer(ctx, driver.RunSpec{Image: id, Entrypoint: []string{"/bin/sh", "-c", "#(nop) x"}})
			if err != nil {
				t.Fatal(err)
			}
			var tb bytes.Buffer
			tw := tar.NewWriter(&tb)
			tw.WriteHeader(&tar.Header{Name: "a/b", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
			tw.Write([]byte("hi"))
			tw.Close()
			if err := d.PutArchive(ctx, c, "/", bytes.NewReader(tb.Bytes())); err != nil {
				t.Fatal(err)
			}
			if ok, err := d.StatPath(ctx, c, "/a/b"); !ok || err != nil {
				t.Fatalf("stat: %v %v", ok, err)
			}
			if ok, err := d.StatPath(ctx, c, "/nope"); ok || err != nil {
				t.Fatalf("stat missing: %v %v", ok, err)
			}
			if _, err := d.GetArchive(ctx, c, "/nope"); !errors.Is(err, driver.ErrNotFound) {
				t.Fatalf("get missing: %v", err)
			}
			rc, err := d.GetArchive(ctx, c, "/a")
			if err != nil {
				t.Fatal(err)
			}
			tr := tar.NewReader(rc)
			var names []string
			for {
				h, err := tr.Next()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(tr)
				names = append(names, h.Name+"="+string(b))
			}
			rc.Close()
			if strings.Join(names, " ") != "a/= a/b=hi" {
				t.Fatalf("archive: %q", names)
			}
			nid, err := d.Commit(ctx, driver.CommitSpec{Container: c, Comment: "c", Config: driver.ImageConfig{Env: []string{"A=1"}, WorkingDir: "/a", Labels: map[string]string{"k": "v"}}})
			if err != nil {
				t.Fatal(err)
			}
			ni, err := d.InspectImage(ctx, nid)
			if err != nil || ni.Layers != 1 || ni.Config.WorkingDir != "/a" || ni.Config.Labels["k"] != "v" {
				t.Fatalf("committed: %+v %v", ni, err)
			}
			if err := d.TagImage(ctx, nid, "lab/x", "v2"); err != nil {
				t.Fatal(err)
			}
			if _, err := d.InspectImage(ctx, "lab/x:v2"); err != nil {
				t.Fatal(err)
			}
			if err := d.TagImage(ctx, "nope", "lab/x", "v3"); !errors.Is(err, driver.ErrNotFound) {
				t.Fatalf("tag missing: %v", err)
			}
			// Running a module the image does not have: 127, as an ExitError.
			r, err := d.CreateContainer(ctx, driver.RunSpec{Image: nid, Entrypoint: []string{"/bin/missing.wasm"}})
			if err != nil {
				t.Fatal(err)
			}
			code, err := d.Run(ctx, r, io.Discard, io.Discard)
			var xe *driver.ExitError
			if code != driver.ExitMissing || !errors.As(err, &xe) {
				t.Fatalf("missing module: %d %v", code, err)
			}
			if err := d.RemoveContainer(ctx, r); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Commit(ctx, driver.CommitSpec{Container: "nope"}); err == nil {
				t.Fatal("commit of a missing container")
			}
		})
	}
}

// countingBody counts the bytes a response body delivers.
type countingBody struct {
	io.ReadCloser
	n *int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	*b.n += int64(n)
	return n, err
}

// TestStatPathTransfersNothing: the WORKDIR check (HEAD on the archive
// endpoint, stat-only since engine v0.14.0) moves no tar, however large
// the directory — while a GET of the same directory does (the control).
func TestStatPathTransfersNothing(t *testing.T) {
	e, err := enginetest.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	var got int64
	inner := e.HTTPClient().Transport
	hc := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := inner.RoundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, "/archive") && r.Method != http.MethodPut {
			resp.Body = countingBody{resp.Body, &got}
		}
		return resp, err
	})}
	_, base := e.API().HTTP()
	d := driver.NewClient(hc, base)
	ctx := context.Background()
	id, err := d.LoadImage(ctx, bytes.NewReader(emptyLayout(t, "")))
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.CreateContainer(ctx, driver.RunSpec{Image: id, Entrypoint: []string{"/bin/sh", "-c", "#(nop)"}})
	if err != nil {
		t.Fatal(err)
	}
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	chunk := bytes.Repeat([]byte("x"), 16<<10)
	for i := 0; i < 512; i++ { // 8 MiB in 512 files
		tw.WriteHeader(&tar.Header{Name: "big/f" + strconv.Itoa(i), Mode: 0o644, Size: int64(len(chunk)), Typeflag: tar.TypeReg})
		tw.Write(chunk)
	}
	tw.Close()
	if err := d.PutArchive(ctx, c, "/", &tb); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ok, err := d.StatPath(ctx, c, "/big")
	took := time.Since(start)
	if !ok || err != nil {
		t.Fatalf("stat: %v %v", ok, err)
	}
	if got != 0 {
		t.Fatalf("HEAD transferred %d bytes", got)
	}
	if took > 2*time.Second {
		t.Fatalf("HEAD took %v", took)
	}
	rc, err := d.GetArchive(ctx, c, "/big")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()
	if got < 8<<20 {
		t.Fatalf("control: GET moved only %d bytes, the counter cannot see a tar", got)
	}
}

// The base may carry the API version (api.Client.HTTP's does) or not;
// requests go to exactly one version prefix either way.
func TestBaseVersionNeverDoubled(t *testing.T) {
	for in, want := range map[string]string{
		"http://wctr": "http://wctr", "http://wctr/": "http://wctr", "http://wctr/v1.47": "http://wctr",
		"http://wctr/v1.47/": "http://wctr", "http://wctr/v1.24": "http://wctr", "http://h/p/v2.0": "http://h/p",
		"http://h/v1.47x": "http://h/v1.47x",
	} {
		if got := driver.StripVersion(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	var paths []string
	hc := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"message":"x"}`)), Header: http.Header{}}, nil
	})}
	for _, base := range []string{"http://wctr", "http://wctr/v1.47"} {
		driver.NewClient(hc, base).InspectImage(context.Background(), "img")
	}
	if len(paths) != 2 || paths[0] != paths[1] || strings.Count(paths[0], "/v1.") != 1 {
		t.Fatalf("paths %q", paths)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDial(t *testing.T) {
	for _, h := range []string{"tcp://x:1", "unix://", "::"} {
		if _, err := driver.Dial(h); err == nil {
			t.Errorf("%q accepted", h)
		}
	}
	c, err := driver.Dial("unix:///nonexistent/wctr.sock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.InspectImage(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "cannot connect") {
		t.Fatalf("got %v", err)
	}
}
