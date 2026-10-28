// Package enginetest is an in-process engine for the builder's tests: the
// real engine (daemon, API server, wazero executor) at the version in
// go.mod, plus a prototype of the endpoints the builder needs and the
// engine does not serve yet:
//
//	POST /commit?container=&repo=&tag=&comment=&author=   body: image config
//	POST /images/{name}/tag?repo=&tag=
//	POST /images/load                                    body: OCI layout tar
//	PUT  /containers/{id}/archive?path=&noOverwriteDirNonDir=
//	GET  /containers/{id}/archive?path=
//	GET  /images/{name}/json                              (full config)
//
// Their shapes are those of the Docker Engine API; the engine is asked to
// implement them (see docs/ENGINE.md). Until it does, this package reaches
// the daemon's image store through reflection — a stopgap pinned to the
// engine version in go.mod, used by tests only.
package enginetest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/api/inproc"
	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime/wazero"

	"github.com/go-wasmcontainers/build/driver"
)

// Limits on what the prototype endpoints accept.
const (
	maxArchive = 256 << 20
	maxConfig  = 4 << 20
)

// Engine is an in-process engine.
type Engine struct {
	D     *daemon.Daemon
	store *image.Store
	exec  *wazero.Executor
	inner http.Handler
	mu    sync.Mutex
	// Calls counts requests per route, for tests.
	Calls map[string]int
}

// New starts an engine whose images live under dir.
func New(dir string) (*Engine, error) {
	ex := wazero.New(wazero.Options{})
	d, err := daemon.New(daemon.Options{Root: dir, Containers: container.Options{Executor: ex}})
	if err != nil {
		ex.Close(context.Background())
		return nil, err
	}
	f := reflect.ValueOf(d).Elem().FieldByName("store")
	if !f.IsValid() {
		return nil, errors.New("enginetest: the daemon has no store field (engine changed): implement the endpoints in the engine")
	}
	st, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(*image.Store)
	if !ok {
		return nil, errors.New("enginetest: unexpected store type")
	}
	return &Engine{D: d, store: st, exec: ex, inner: server.New(d, server.Info{Version: "enginetest"}), Calls: map[string]int{}}, nil
}

// Close stops the engine.
func (e *Engine) Close() {
	e.D.Close(context.Background())
	e.exec.Close(context.Background())
}

// HTTPClient reaches the engine in-process.
func (e *Engine) HTTPClient() *http.Client {
	return &http.Client{Transport: &inproc.Transport{Handler: e}}
}

// Driver is a builder driver for this engine.
func (e *Engine) Driver() *driver.Client { return driver.NewClient(e.HTTPClient(), "http://wctr") }

// API is the engine's own client.
func (e *Engine) API() *api.Client { return api.NewClientWith(e.HTTPClient(), "http://wctr") }

func (e *Engine) count(route string) {
	e.mu.Lock()
	e.Calls[route]++
	e.mu.Unlock()
}

// ServeHTTP routes the prototype endpoints and passes the rest on.
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if strings.HasPrefix(p, "/v1.") {
		if i := strings.IndexByte(p[1:], '/'); i > 0 {
			p = p[i+1:]
		}
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && p == "/commit":
		e.count("commit")
		e.commit(w, r)
	case r.Method == http.MethodPost && p == "/images/load":
		e.count("load")
		e.load(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/tag"):
		e.count("tag")
		e.tag(w, strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/tag"), q.Get("repo"), q.Get("tag"))
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
		e.count("inspect")
		e.inspect(w, strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json"))
	case strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/archive"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/archive")
		switch r.Method {
		case http.MethodPut:
			e.count("archive-put")
			e.putArchive(w, r, id, q.Get("path"), q.Get("noOverwriteDirNonDir") != "")
		case http.MethodHead:
			e.count("archive-head")
			c, err := e.D.Containers().Get(id)
			if err != nil {
				fail(w, http.StatusNotFound, err)
				return
			}
			if _, err := c.FS().Lstat(path.Clean("/" + q.Get("path"))); err != nil {
				fail(w, http.StatusNotFound, err)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			e.count("archive-get")
			e.getArchive(w, id, q.Get("path"))
		}
	default:
		e.count(r.Method + " " + p)
		e.inner.ServeHTTP(w, r)
	}
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(api.ErrorResponse{Message: err.Error()})
}

func (e *Engine) rawConfig(img *image.Image) (map[string]any, error) {
	b, err := e.store.ReadBlob(img.ConfigDigest, maxConfig)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func (e *Engine) inspect(w http.ResponseWriter, ref string) {
	_, img, err := e.D.ResolveImage(ref)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	raw, err := e.rawConfig(img)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{
		"Id": string(img.ID()), "Os": raw["os"], "Architecture": raw["architecture"], "Author": raw["author"],
		"Created": raw["created"], "Config": raw["config"], "History": raw["history"],
		"RootFS": map[string]any{"Type": "layers", "Layers": img.Config.RootFS.DiffIDs},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// name turns repo[:tag] into the store's name.
func storeName(repo, tag string) (string, error) {
	if tag != "" {
		repo += ":" + tag
	}
	return image.NormalizeName(repo)
}

// put stores an image (blobs must be ingested) under name, or under a
// hidden name when name is "" (the store resolves IDs only through names).
func (e *Engine) put(name string, manifest []byte) (string, error) {
	md := image.FromBytes(manifest)
	if err := e.store.Ingest(image.Descriptor{Digest: md, Size: int64(len(manifest))}, bytes.NewReader(manifest)); err != nil {
		return "", err
	}
	if name == "" {
		var err error
		if name, err = image.NormalizeName("wctr-build-dangling/" + md.Encoded()[:12]); err != nil {
			return "", err
		}
	}
	if err := e.store.Tag(name, image.Ref{Manifest: md, RepoDigest: md}); err != nil {
		return "", err
	}
	img, err := e.store.Image(md, image.DefaultLimits)
	if err != nil {
		return "", err
	}
	return string(img.ID()), nil
}

func (e *Engine) ingest(b []byte, mediaType string) image.Descriptor {
	d := image.Descriptor{MediaType: mediaType, Digest: image.FromBytes(b), Size: int64(len(b))}
	e.store.Ingest(d, bytes.NewReader(b))
	return d
}

func (e *Engine) commit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := e.D.Containers().Get(q.Get("container"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	var cfg json.RawMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, maxConfig)).Decode(&cfg); err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("commit body: %w", err))
		return
	}
	snap := c.Snapshot()
	_, parent, err := e.D.ResolveImage(snap.Config.ImageID)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	raw, err := e.rawConfig(parent)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	hist := map[string]any{"created": now, "created_by": strings.Join(append(append([]string{}, snap.Config.Entrypoint...), snap.Config.Cmd...), " ")}
	if v := q.Get("comment"); v != "" {
		hist["comment"] = v
	}
	if v := q.Get("author"); v != "" {
		hist["author"] = v
		raw["author"] = v
	}
	layers := append([]image.Descriptor{}, parent.Manifest.Layers...)
	rootfsM, _ := raw["rootfs"].(map[string]any)
	if rootfsM == nil {
		rootfsM = map[string]any{"type": "layers"}
	}
	diffIDs, _ := rootfsM["diff_ids"].([]any)
	if len(c.FS().Diff()) == 0 {
		hist["empty_layer"] = true
	} else {
		var buf bytes.Buffer
		if _, err := c.FS().WriteDiff(&buf); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		d := e.ingest(buf.Bytes(), image.MediaTypeOCILayer)
		layers = append(layers, d)
		diffIDs = append(diffIDs, string(d.Digest))
	}
	if diffIDs == nil {
		diffIDs = []any{}
	}
	rootfsM["diff_ids"] = diffIDs
	raw["rootfs"] = rootfsM
	raw["config"] = cfg
	raw["created"] = now
	h, _ := raw["history"].([]any)
	raw["history"] = append(h, hist)
	cb, _ := json.Marshal(raw)
	cd := e.ingest(cb, image.MediaTypeOCIConfig)
	man, _ := json.Marshal(image.Manifest{SchemaVersion: 2, MediaType: image.MediaTypeOCIManifest, Config: cd, Layers: layers})
	name := ""
	if repo := q.Get("repo"); repo != "" {
		if name, err = storeName(repo, q.Get("tag")); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	}
	id, err := e.put(name, man)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"Id": id})
}

func (e *Engine) tag(w http.ResponseWriter, ref, repo, tag string) {
	_, img, err := e.D.ResolveImage(ref)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	name, err := storeName(repo, tag)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := e.store.Tag(name, image.Ref{Manifest: img.ManifestDigest, RepoDigest: img.ManifestDigest}); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// load imports an OCI image layout tar (the image named by
// org.opencontainers.image.ref.name, or the only one, dangling).
func (e *Engine) load(w http.ResponseWriter, r *http.Request) {
	files := map[string][]byte{}
	tr := tar.NewReader(io.LimitReader(r.Body, maxArchive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxArchive))
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		files[path.Clean(h.Name)] = b
	}
	var idx image.Index
	if err := json.Unmarshal(files["index.json"], &idx); err != nil || len(idx.Manifests) != 1 {
		fail(w, http.StatusBadRequest, errors.New("load: want an OCI layout with one image"))
		return
	}
	for n, b := range files {
		if hex, ok := strings.CutPrefix(n, "blobs/sha256/"); ok {
			d := image.Digest("sha256:" + hex)
			if err := e.store.Ingest(image.Descriptor{Digest: d, Size: int64(len(b))}, bytes.NewReader(b)); err != nil {
				fail(w, http.StatusBadRequest, err)
				return
			}
		}
	}
	md := idx.Manifests[0]
	man, ok := files["blobs/sha256/"+md.Digest.Encoded()]
	if !ok {
		fail(w, http.StatusBadRequest, errors.New("load: manifest blob missing"))
		return
	}
	name := ""
	if n := md.Annotations[image.AnnotationRefName]; n != "" {
		var err error
		if name, err = image.NormalizeName(n); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	}
	id, err := e.put(name, man)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	msg := "Loaded image ID: " + id + "\n"
	if name != "" {
		msg = "Loaded image: " + name + "\n"
	}
	json.NewEncoder(w).Encode(map[string]string{"stream": msg})
}

func (e *Engine) putArchive(w http.ResponseWriter, r *http.Request, id, dir string, noOverwrite bool) {
	c, err := e.D.Containers().Get(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	if err := applyTar(c.FS(), path.Clean("/"+dir), io.LimitReader(r.Body, maxArchive), noOverwrite); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// applyTar extracts a tar below dir: names are cleaned and kept inside;
// missing parents are created 0755; with noOverwrite a directory is never
// replaced by a non-directory or the reverse.
func applyTar(f *rootfs.FS, dir string, r io.Reader, noOverwrite bool) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean("/" + h.Name)
		p := path.Join(dir, name)
		if p == "/" {
			continue
		}
		if err := f.MkdirAll(path.Dir(p), 0o755); err != nil {
			return err
		}
		old, statErr := f.Lstat(p)
		if statErr == nil && noOverwrite && old.IsDir() != (h.Typeflag == tar.TypeDir) {
			return fmt.Errorf("cannot overwrite %s %s with a %s", map[bool]string{true: "directory", false: "non-directory"}[old.IsDir()], p, map[bool]string{true: "directory", false: "non-directory"}[h.Typeflag == tar.TypeDir])
		}
		mode := fs.FileMode(h.Mode) & fs.ModePerm
		switch h.Typeflag {
		case tar.TypeDir:
			if statErr != nil {
				if err := f.Mkdir(p, mode); err != nil {
					return err
				}
			}
			if err := f.Chmod(p, mode); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			b, err := io.ReadAll(io.LimitReader(tr, maxArchive))
			if err != nil {
				return err
			}
			if statErr == nil && !old.Mode().IsRegular() {
				if err := f.Remove(p); err != nil {
					return err
				}
			}
			if err := f.WriteFile(p, b, mode); err != nil {
				return err
			}
			if err := f.Chmod(p, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if statErr == nil {
				if err := f.Remove(p); err != nil {
					return err
				}
			}
			if err := f.Symlink(h.Linkname, p); err != nil {
				return err
			}
			continue
		default:
			continue
		}
		f.Chtimes(p, h.ModTime, false)
	}
}

func (e *Engine) getArchive(w http.ResponseWriter, id, p string) {
	c, err := e.D.Containers().Get(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	p = path.Clean("/" + p)
	fsys := c.FS()
	if _, err := fsys.Lstat(p); err != nil {
		fail(w, http.StatusNotFound, fmt.Errorf("Could not find the file %s in container %s", p, id))
		return
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	root := path.Base(p)
	if p == "/" {
		root = ""
	}
	var walk func(abs, name string) error
	walk = func(abs, name string) error {
		info, err := fsys.Lstat(abs)
		if err != nil {
			return err
		}
		if name != "" {
			h := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(), Format: tar.FormatPAX}
			switch {
			case info.IsDir():
				h.Typeflag, h.Name = tar.TypeDir, name+"/"
			case info.Mode()&fs.ModeSymlink != 0:
				t, err := fsys.Readlink(abs)
				if err != nil {
					return err
				}
				h.Typeflag, h.Linkname = tar.TypeSymlink, t
			case info.Mode().IsRegular():
				h.Typeflag, h.Size = tar.TypeReg, info.Size()
			default:
				return nil
			}
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if h.Typeflag == tar.TypeReg {
				b, err := fsys.ReadFile(abs)
				if err != nil {
					return err
				}
				tw.Write(b)
			}
		}
		if info.IsDir() {
			ents, err := fsys.ReadDir(abs)
			if err != nil {
				return err
			}
			for _, d := range ents {
				if err := walk(path.Join(abs, d.Name()), path.Join(name, d.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(p, root); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	tw.Close()
	w.Header().Set("Content-Type", "application/x-tar")
	w.Write(buf.Bytes())
}

// Digest of bytes, for tests.
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}
