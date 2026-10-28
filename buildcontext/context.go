// Package buildcontext holds a build context: the files a Containerfile's
// COPY and ADD read, received as a tar stream (from wctr, or from a stage
// or image through the engine) and kept in memory. Nothing in it ever names
// a host path: a build reads only what was sent.
//
// The stream is untrusted. Names are cleaned and must stay inside the
// context (no absolute names, no ".."); the total size, the number of
// entries, name length and depth are bounded; symbolic links are kept as
// links and only followed, when a source path goes through one, inside the
// context — a link that leads out of it is an error.
package buildcontext

import (
	"archive/tar"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/go-wasmcontainers/build/ignore"
)

// Limits bound a context.
type Limits struct {
	MaxBytes   int64 // file content, all files together
	MaxFiles   int   // entries
	MaxPathLen int
	MaxDepth   int
}

// DefaultLimits suit a training lab and match the engine's default
// container quota.
var DefaultLimits = Limits{MaxBytes: 256 << 20, MaxFiles: 65536, MaxPathLen: 4096, MaxDepth: 128}

// ErrLimit reports a context beyond its limits.
var ErrLimit = errors.New("build context exceeds its limits")

// ErrEscape reports a path or link that leaves the context.
var ErrEscape = errors.New("path leaves the build context")

// Entry is one file, directory or symbolic link.
type Entry struct {
	Mode     fs.FileMode // type bits and permissions
	Data     []byte
	Link     string // symlink target
	ModTime  time.Time
	UID, GID int
	// Sum is the sha256 of Data (regular files).
	Sum [32]byte
}

// Context is an in-memory file tree. Paths are slash-separated, relative,
// cleaned ("." is the root).
type Context struct {
	entries map[string]*Entry
	bytes   int64
	lim     Limits
}

// New returns an empty context.
func New(lim Limits) *Context {
	if lim.MaxFiles <= 0 {
		lim = DefaultLimits
	}
	return &Context{entries: map[string]*Entry{".": {Mode: fs.ModeDir | 0o755}}, lim: lim}
}

// CleanName turns a tar entry name into a context path, or fails when it
// would leave the context.
func CleanName(name string, lim Limits) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: NUL in %q", ErrEscape, name)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("%w: absolute name %q", ErrEscape, name)
	}
	p := path.Clean(name)
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("%w: %q", ErrEscape, name)
	}
	if len(p) > lim.MaxPathLen || strings.Count(p, "/") >= lim.MaxDepth {
		return "", fmt.Errorf("%w: name %.64q… too long or too deep", ErrLimit, p)
	}
	return p, nil
}

// Add stores an entry (creating missing parent directories) and charges it
// to the limits.
func (c *Context) Add(p string, e *Entry) error {
	p, err := CleanName(p, c.lim)
	if err != nil {
		return err
	}
	if p == "." {
		if e.Mode.IsDir() {
			c.entries["."].ModTime = e.ModTime
			return nil
		}
		return fmt.Errorf("%w: the root must be a directory", ErrEscape)
	}
	if err := c.mkdirs(path.Dir(p), e.ModTime); err != nil {
		return err
	}
	if old, ok := c.entries[p]; ok {
		c.bytes -= int64(len(old.Data))
		if old.Mode.IsDir() && !e.Mode.IsDir() {
			c.removeTree(p)
		}
	} else if len(c.entries) >= c.lim.MaxFiles {
		return fmt.Errorf("%w: more than %d entries", ErrLimit, c.lim.MaxFiles)
	}
	c.bytes += int64(len(e.Data))
	if c.bytes > c.lim.MaxBytes {
		return fmt.Errorf("%w: more than %d bytes", ErrLimit, c.lim.MaxBytes)
	}
	if e.Mode.IsRegular() {
		e.Sum = sha256.Sum256(e.Data)
	}
	c.entries[p] = e
	return nil
}

func (c *Context) mkdirs(dir string, mt time.Time) error {
	if dir == "." {
		return nil
	}
	if e, ok := c.entries[dir]; ok {
		if !e.Mode.IsDir() {
			return fmt.Errorf("%s: a parent of an entry is not a directory", dir)
		}
		return nil
	}
	if err := c.mkdirs(path.Dir(dir), mt); err != nil {
		return err
	}
	if len(c.entries) >= c.lim.MaxFiles {
		return fmt.Errorf("%w: more than %d entries", ErrLimit, c.lim.MaxFiles)
	}
	c.entries[dir] = &Entry{Mode: fs.ModeDir | 0o755, ModTime: mt}
	return nil
}

func (c *Context) removeTree(p string) {
	for k, e := range c.entries {
		if k == p || strings.HasPrefix(k, p+"/") {
			c.bytes -= int64(len(e.Data))
			delete(c.entries, k)
		}
	}
}

// Len is the number of entries, the root included.
func (c *Context) Len() int { return len(c.entries) }

// Bytes is the total file content.
func (c *Context) Bytes() int64 { return c.bytes }

// FromTar reads a tar stream into a context, each name placed under
// prefix ("" or "." for the root). Hard links become copies of their
// target; devices, FIFOs and other types are skipped.
func FromTar(r io.Reader, lim Limits, prefix string) (*Context, error) {
	c := New(lim)
	if err := c.ReadTar(r, prefix); err != nil {
		return nil, err
	}
	return c, nil
}

// ReadTar adds a tar stream's entries under prefix.
func (c *Context) ReadTar(r io.Reader, prefix string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("build context: %w", err)
		}
		name, err := CleanName(h.Name, c.lim)
		if err != nil {
			return err
		}
		if prefix != "" && prefix != "." {
			name = path.Join(prefix, name)
		}
		e := &Entry{ModTime: h.ModTime, UID: h.Uid, GID: h.Gid}
		perm := fs.FileMode(h.Mode) & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		switch h.Typeflag {
		case tar.TypeDir:
			e.Mode = fs.ModeDir | perm
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 || c.bytes+h.Size > c.lim.MaxBytes {
				return fmt.Errorf("%w: more than %d bytes", ErrLimit, c.lim.MaxBytes)
			}
			data, err := io.ReadAll(io.LimitReader(tr, h.Size))
			if err != nil {
				return fmt.Errorf("build context: %s: %w", h.Name, err)
			}
			if int64(len(data)) != h.Size {
				return fmt.Errorf("build context: %s: truncated", h.Name)
			}
			e.Mode, e.Data = perm, data
		case tar.TypeSymlink:
			if h.Linkname == "" || strings.ContainsRune(h.Linkname, 0) || len(h.Linkname) > c.lim.MaxPathLen {
				return fmt.Errorf("build context: %s: bad link target", h.Name)
			}
			e.Mode, e.Link = fs.ModeSymlink|0o777, h.Linkname
		case tar.TypeLink:
			target, err := CleanName(h.Linkname, c.lim)
			if err != nil {
				return err
			}
			if prefix != "" && prefix != "." {
				target = path.Join(prefix, target)
			}
			t, ok := c.entries[target]
			if !ok || !t.Mode.IsRegular() {
				return fmt.Errorf("build context: %s: hard link to a missing or non-regular file", h.Name)
			}
			cp := *t
			cp.ModTime, cp.UID, cp.GID = e.ModTime, e.UID, e.GID
			e = &cp
		default:
			continue
		}
		if err := c.Add(name, e); err != nil {
			return err
		}
	}
}

// Exclude removes every entry the matcher excludes. Directories that are
// excluded but hold a re-included entry are kept (as their parents).
func (c *Context) Exclude(m *ignore.Matcher) {
	if m == nil {
		return
	}
	keepDirs := map[string]bool{}
	var drop []string
	for p := range c.entries {
		if p == "." {
			continue
		}
		if m.Match(p) {
			drop = append(drop, p)
			continue
		}
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			keepDirs[d] = true
		}
	}
	for _, p := range drop {
		if keepDirs[p] && c.entries[p].Mode.IsDir() {
			continue
		}
		c.bytes -= int64(len(c.entries[p].Data))
		delete(c.entries, p)
	}
}

// Get returns the entry at p, not following a final symlink.
func (c *Context) Get(p string) (*Entry, bool) {
	e, ok := c.entries[p]
	return e, ok
}

// Resolve follows symbolic links in p's components (and in its last one
// when follow) and returns the context path they lead to. A link that
// would leave the context is ErrEscape.
func (c *Context) Resolve(p string, follow bool) (string, error) {
	p, err := CleanName(strings.TrimPrefix(p, "/"), c.lim)
	if err != nil {
		return "", err
	}
	hops := 0
	var comps []string
	if p != "." {
		comps = strings.Split(p, "/")
	}
	cur := "."
	for i := 0; i < len(comps); i++ {
		next := path.Join(cur, comps[i])
		e, ok := c.entries[next]
		last := i == len(comps)-1
		if ok && e.Mode&fs.ModeSymlink != 0 && (!last || follow) {
			hops++
			if hops > 40 {
				return "", fmt.Errorf("%s: too many levels of symbolic links", p)
			}
			if strings.HasPrefix(e.Link, "/") {
				return "", fmt.Errorf("%w: %s is a link to the absolute path %s", ErrEscape, next, e.Link)
			}
			target := path.Join(cur, e.Link)
			if target == ".." || strings.HasPrefix(target, "../") {
				return "", fmt.Errorf("%w: %s links to %s", ErrEscape, next, e.Link)
			}
			rest := comps[i+1:]
			comps = nil
			if target != "." {
				comps = strings.Split(target, "/")
			}
			comps = append(comps, rest...)
			cur, i = ".", -1
			continue
		}
		cur = next
	}
	return cur, nil
}

// Paths returns every path in sorted order (the root excluded).
func (c *Context) Paths() []string {
	out := make([]string, 0, len(c.entries))
	for p := range c.entries {
		if p != "." {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Under returns p and every path below it, sorted.
func (c *Context) Under(p string) []string {
	var out []string
	for k := range c.entries {
		if k == p || p == "." || strings.HasPrefix(k, p+"/") {
			if k != "." {
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// HasMeta reports whether a COPY source is a wildcard pattern.
func HasMeta(p string) bool { return strings.ContainsAny(p, `*?[\`) }

// Glob returns the paths matching a filepath.Match pattern (each
// component matched at its own depth), sorted.
func (c *Context) Glob(pattern string) ([]string, error) {
	pattern = path.Clean(strings.TrimPrefix(pattern, "/"))
	if _, err := path.Match(pattern, ""); err != nil {
		return nil, fmt.Errorf("bad pattern %q: %w", pattern, err)
	}
	var out []string
	for _, p := range c.Paths() {
		if ok, _ := path.Match(pattern, p); ok {
			out = append(out, p)
		}
	}
	return out, nil
}
