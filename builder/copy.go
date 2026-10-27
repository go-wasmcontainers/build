package builder

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/go-wasmcontainers/build/buildcontext"
)

// owner is a --chown value: numeric only (there is no /etc/passwd lookup).
type owner struct{ uid, gid int }

func parseChown(s string) (*owner, error) {
	if s == "" {
		return nil, nil
	}
	u, g, hasG := strings.Cut(s, ":")
	num := func(x string) (int, error) {
		if x == "root" {
			return 0, nil
		}
		n, err := strconv.Atoi(x)
		if err != nil || n < 0 || n > 1<<31-2 {
			return 0, fmt.Errorf("--chown=%s: user and group must be numeric (or root): there is no /etc/passwd lookup yet", s)
		}
		return n, nil
	}
	uid, err := num(u)
	if err != nil {
		return nil, err
	}
	gid := uid
	if hasG {
		if gid, err = num(g); err != nil {
			return nil, err
		}
	}
	return &owner{uid, gid}, nil
}

func parseChmod(s string) (*fs.FileMode, error) {
	if s == "" {
		return nil, nil
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil || n > 0o7777 {
		return nil, fmt.Errorf("--chmod=%s: only octal modes (e.g. 755) are supported", s)
	}
	m := fs.FileMode(n & 0o777)
	if n&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if n&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if n&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return &m, nil
}

// copyOp is a resolved COPY or ADD.
type copyOp struct {
	src     *buildcontext.Context
	srcs    []string // as written, expanded
	dest    string   // absolute, trailing "/" kept
	chown   *owner
	chmod   *fs.FileMode
	unpack  bool // ADD: extract local tar archives
	what    string
	ignored bool // the context had an ignore file (for messages)
	// fromContext: files from the build context belong to root unless
	// --chown says otherwise (owners are never taken from the client's
	// host); files from a stage or image keep theirs.
	fromContext bool
}

// layer is a computed change set.
type layer struct {
	tar    []byte
	digest string // over names, modes, owners, links and content; not mtimes
	files  int
}

type item struct {
	name string // relative to "/"
	e    *buildcontext.Entry
}

func isDirDest(d string) bool {
	return strings.HasSuffix(d, "/") || path.Base(d) == "." || path.Base(d) == ".."
}

// resolveDest makes dest absolute against workdir, keeping a trailing "/".
func resolveDest(dest, workdir string) string {
	dir := isDirDest(dest)
	if !path.IsAbs(dest) {
		dest = path.Join("/", workdir, dest)
	}
	dest = path.Clean(dest)
	if dir && dest != "/" {
		dest += "/"
	}
	return dest
}

// build computes the layer of a COPY or ADD from its source context.
func (op *copyOp) build(lim buildcontext.Limits) (*layer, error) {
	type match struct{ given, resolved string }
	var matches []match
	for _, s := range op.srcs {
		rel := strings.TrimPrefix(path.Clean("/"+s), "/")
		if rel == "" {
			rel = "."
		}
		var cands []string
		if buildcontext.HasMeta(s) {
			g, err := op.src.Glob(rel)
			if err != nil {
				return nil, err
			}
			cands = g
		} else {
			cands = []string{rel}
		}
		for _, c := range cands {
			r, err := op.src.Resolve(c, true)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", op.what, s, err)
			}
			if _, ok := op.src.Get(r); !ok {
				msg := fmt.Sprintf("%s failed: file not found in build context", op.what)
				if op.ignored {
					msg += " or excluded by the ignore file"
				}
				return nil, fmt.Errorf("%s: %s", msg, s)
			}
			matches = append(matches, match{c, r})
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%s failed: no source files were specified (no match for %s)", op.what, strings.Join(op.srcs, " "))
	}
	dirDest := isDirDest(op.dest)
	if len(matches) > 1 && !dirDest {
		return nil, fmt.Errorf("when using %s with more than one source file, the destination must be a directory and end with a /", op.what)
	}
	base := strings.TrimSuffix(op.dest, "/")
	out := map[string]*buildcontext.Entry{}
	var total int64
	put := func(name string, e *buildcontext.Entry) error {
		name = strings.TrimPrefix(path.Clean(name), "/")
		if name == "" || name == "." {
			return nil // the root directory itself is never rewritten
		}
		if len(out) >= lim.MaxFiles {
			return fmt.Errorf("%w: more than %d entries in one %s", buildcontext.ErrLimit, lim.MaxFiles, op.what)
		}
		total += int64(len(e.Data))
		if total > lim.MaxBytes {
			return fmt.Errorf("%w: more than %d bytes in one %s", buildcontext.ErrLimit, lim.MaxBytes, op.what)
		}
		out[name] = e
		return nil
	}
	for _, m := range matches {
		e, _ := op.src.Get(m.resolved)
		switch {
		case e.Mode.IsDir():
			if err := put(base, e); err != nil {
				return nil, err
			}
			for _, q := range op.src.Under(m.resolved) {
				if q == m.resolved {
					continue
				}
				rel := strings.TrimPrefix(q, m.resolved+"/")
				if m.resolved == "." {
					rel = q
				}
				qe, _ := op.src.Get(q)
				if err := put(path.Join(base, rel), qe); err != nil {
					return nil, err
				}
			}
		case op.unpack && e.Mode.IsRegular() && isArchive(e.Data):
			if err := extractInto(e.Data, base, lim, put); err != nil {
				return nil, fmt.Errorf("%s %s: %w", op.what, m.given, err)
			}
		default:
			target := base
			if dirDest {
				target = path.Join(base, path.Base(m.given))
			}
			if err := put(target, e); err != nil {
				return nil, err
			}
		}
	}
	return op.writeTar(out)
}

func (op *copyOp) writeTar(entries map[string]*buildcontext.Entry) (*layer, error) {
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	h := sha256.New()
	for _, n := range names {
		e := entries[n]
		mode := e.Mode
		if op.chmod != nil && mode&fs.ModeSymlink == 0 {
			mode = mode&fs.ModeType | *op.chmod
		}
		uid, gid := e.UID, e.GID
		if op.chown != nil {
			uid, gid = op.chown.uid, op.chown.gid
		} else if op.fromContext {
			uid, gid = 0, 0
		}
		th := &tar.Header{Name: n, Mode: int64(mode.Perm()), Uid: uid, Gid: gid, ModTime: e.ModTime, Format: tar.FormatPAX}
		if mode&fs.ModeSetuid != 0 {
			th.Mode |= 0o4000
		}
		if mode&fs.ModeSetgid != 0 {
			th.Mode |= 0o2000
		}
		if mode&fs.ModeSticky != 0 {
			th.Mode |= 0o1000
		}
		switch {
		case mode.IsDir():
			th.Typeflag, th.Name = tar.TypeDir, n+"/"
		case mode&fs.ModeSymlink != 0:
			th.Typeflag, th.Linkname = tar.TypeSymlink, e.Link
		default:
			th.Typeflag, th.Size = tar.TypeReg, int64(len(e.Data))
		}
		fmt.Fprintf(h, "%s\x00%o\x00%d:%d\x00%s\x00%x\n", th.Name, th.Mode, uid, gid, th.Linkname, e.Sum)
		if err := tw.WriteHeader(th); err != nil {
			return nil, err
		}
		if th.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.Data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &layer{tar: buf.Bytes(), digest: hex.EncodeToString(h.Sum(nil)), files: len(names)}, nil
}

// isArchive recognises a tar archive by content, as the reference says
// ADD does: gzip and bzip2 compressed or plain (ustar magic).
func isArchive(b []byte) bool {
	switch {
	case len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b:
		return true
	case len(b) > 3 && string(b[:3]) == "BZh":
		return true
	case len(b) > 262 && string(b[257:262]) == "ustar":
		return true
	}
	return false
}

var errUnsupportedCompression = errors.New("xz and zstd archives are not supported by ADD yet")

// extractInto unpacks an archive below dir, bounded by lim (the expanded
// size counts, so a compression bomb stops at MaxBytes).
func extractInto(data []byte, dir string, lim buildcontext.Limits, put func(string, *buildcontext.Entry) error) error {
	var r io.Reader = bytes.NewReader(data)
	switch {
	case data[0] == 0x1f && data[1] == 0x8b:
		zr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		zr.Multistream(false)
		r = zr
	case string(data[:3]) == "BZh":
		r = bzip2.NewReader(r)
	case len(data) > 4 && (string(data[:4]) == "\x28\xb5\x2f\xfd" || string(data[:4]) == "\xfd7zX"):
		return errUnsupportedCompression
	}
	r = &capReader{r: bufio.NewReader(r), left: lim.MaxBytes + 1}
	sub, err := buildcontext.FromTar(r, lim, "")
	if err != nil {
		return err
	}
	for _, p := range sub.Paths() {
		e, _ := sub.Get(p)
		if err := put(path.Join(dir, p), e); err != nil {
			return err
		}
	}
	return nil
}

type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, buildcontext.ErrLimit
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
