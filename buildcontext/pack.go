package buildcontext

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/go-wasmcontainers/build/ignore"
)

// IgnoreFiles are looked for at the context root, in this order
// (containerignore(5)): the first one found is used.
var IgnoreFiles = []string{".containerignore", ".dockerignore"}

// ReadIgnore reads the context's ignore file from fsys, if any; the name
// found is returned ("" for none).
func ReadIgnore(fsys fs.FS) (*ignore.Matcher, string, error) {
	for _, n := range IgnoreFiles {
		f, err := fsys.Open(n)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		pats, err := ignore.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", n, err)
		}
		m, err := ignore.New(pats)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", n, err)
		}
		return m, n, nil
	}
	return nil, "", nil
}

// IgnoreMatcher reads the context's own ignore file, if it holds one.
func (c *Context) IgnoreMatcher() (*ignore.Matcher, error) {
	for _, n := range IgnoreFiles {
		e, ok := c.entries[n]
		if !ok || !e.Mode.IsRegular() {
			continue
		}
		pats, err := ignore.ReadAll(bytes.NewReader(e.Data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		m, err := ignore.New(pats)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		return m, nil
	}
	return nil, nil
}

// PackStats says what was sent.
type PackStats struct {
	Files int
	Bytes int64
}

// Pack writes the directory dir as a context tar to w, leaving out what
// its ignore file excludes. The directory is opened as an os.Root, so no
// file outside it is ever read, whatever links it holds: symbolic links
// are sent as links, never followed. Owners are not sent (every entry is
// 0:0) and neither are devices, FIFOs or sockets.
func Pack(dir string, w io.Writer, lim Limits) (PackStats, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return PackStats{}, err
	}
	defer root.Close()
	return PackFS(root.FS(), root.Readlink, w, lim)
}

// PackFS is Pack over any file system; readlink reads a link's target.
func PackFS(fsys fs.FS, readlink func(string) (string, error), w io.Writer, lim Limits) (PackStats, error) {
	if lim.MaxFiles <= 0 {
		lim = DefaultLimits
	}
	m, _, err := ReadIgnore(fsys)
	if err != nil {
		return PackStats{}, err
	}
	tw := tar.NewWriter(w)
	var st PackStats
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if _, err := CleanName(p, lim); err != nil {
			return err
		}
		if m.Match(p) {
			if d.IsDir() && !m.Exclusions() {
				return fs.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
			// An exception may re-include something below: descend, and
			// send the directory only if something inside is sent (the
			// receiver creates parents itself).
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h := &tar.Header{Name: p, ModTime: info.ModTime(), Mode: int64(info.Mode().Perm()), Format: tar.FormatPAX}
		switch {
		case info.Mode().IsDir():
			h.Typeflag, h.Name = tar.TypeDir, p+"/"
		case info.Mode()&fs.ModeSymlink != 0:
			t, err := readlink(p)
			if err != nil {
				return err
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, t
		case info.Mode().IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, info.Size()
		default:
			return nil
		}
		st.Files++
		if st.Files > lim.MaxFiles {
			return fmt.Errorf("%w: more than %d entries", ErrLimit, lim.MaxFiles)
		}
		st.Bytes += h.Size
		if st.Bytes > lim.MaxBytes {
			return fmt.Errorf("%w: more than %d bytes", ErrLimit, lim.MaxBytes)
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			f, err := fsys.Open(p)
			if err != nil {
				return err
			}
			n, err := io.Copy(tw, io.LimitReader(f, h.Size))
			f.Close()
			if err != nil {
				return err
			}
			if n != h.Size {
				return fmt.Errorf("%s changed size while it was sent", p)
			}
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	return st, tw.Close()
}
