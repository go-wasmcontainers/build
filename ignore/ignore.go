// Package ignore reads .containerignore / .dockerignore files and matches
// build-context paths against them, with the semantics of
// containerignore(5) and moby/patternmatcher: Go filepath.Match syntax
// plus "**" (any number of directories, including none), "!" exceptions
// where the last matching line wins, patterns cleaned with path.Clean and
// a leading "/" dropped, and a path excluded when it or any of its parent
// directories matches.
//
// Paths are slash-separated and relative to the context root on every
// platform: the context is a tar stream, not a host path.
package ignore

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

// Limits on an ignore file (it is untrusted input).
const (
	MaxFileBytes = 1 << 20
	MaxPatterns  = 4096
)

// ReadAll reads an ignore file: a UTF-8 BOM is stripped, lines starting
// with "#" are comments, blanks are trimmed, each pattern is cleaned and a
// leading "/" removed ("!" kept in front).
func ReadAll(r io.Reader) ([]string, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("ignore file larger than %d bytes", MaxFileBytes)
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), MaxFileBytes)
	for sc.Scan() {
		p := sc.Text()
		if strings.HasPrefix(p, "#") {
			continue
		}
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		invert := p[0] == '!'
		if invert {
			p = strings.TrimSpace(p[1:])
		}
		if p != "" {
			p = path.Clean(p)
			if len(p) > 1 && p[0] == '/' {
				p = p[1:]
			}
		}
		if invert {
			p = "!" + p
		}
		out = append(out, p)
		if len(out) > MaxPatterns {
			return nil, fmt.Errorf("ignore file has more than %d patterns", MaxPatterns)
		}
	}
	return out, sc.Err()
}

type kind int

const (
	exact kind = iota
	prefix
	suffix
	re
)

type pattern struct {
	text      string
	exclusion bool
	kind      kind
	re        *regexp.Regexp
}

// Matcher matches paths against a pattern list.
type Matcher struct {
	pats       []*pattern
	exclusions bool
}

// New compiles patterns.
func New(patterns []string) (*Matcher, error) {
	if len(patterns) > MaxPatterns {
		return nil, fmt.Errorf("more than %d ignore patterns", MaxPatterns)
	}
	m := &Matcher{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = path.Clean(p)
		np := &pattern{}
		if p[0] == '!' {
			if len(p) == 1 {
				return nil, errors.New(`illegal exclusion pattern: "!"`)
			}
			np.exclusion = true
			p = p[1:]
			m.exclusions = true
		}
		if _, err := path.Match(p, "."); err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		np.text = p
		if err := np.compile(); err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		m.pats = append(m.pats, np)
	}
	return m, nil
}

// Exclusions reports whether any pattern is an exception ("!"): a walker
// must then descend into excluded directories, since something inside may
// be re-included.
func (m *Matcher) Exclusions() bool { return m != nil && m.exclusions }

func (p *pattern) compile() error {
	var b strings.Builder
	b.WriteByte('^')
	k := exact
	rs := []rune(p.text)
	for i := 0; i < len(rs); i++ {
		ch := rs[i]
		switch {
		case ch == '*':
			start := i
			if i+1 < len(rs) && rs[i+1] == '*' {
				i++
				if i+1 < len(rs) && rs[i+1] == '/' {
					i++
				}
				if i+1 >= len(rs) {
					if k == exact {
						k = prefix
					} else {
						b.WriteString(".*")
						k = re
					}
				} else {
					b.WriteString("(.*/)?")
					k = re
				}
				if start == 0 {
					k = suffix
				}
			} else {
				b.WriteString("[^/]*")
				k = re
			}
		case ch == '?':
			b.WriteString("[^/]")
			k = re
		case strings.ContainsRune(".+()|{}$", ch):
			b.WriteString(`\` + string(ch))
		case ch == '\\':
			if i+1 < len(rs) {
				i++
				b.WriteString(regexp.QuoteMeta(string(rs[i])))
				k = re
			} else {
				b.WriteString(`\\`)
			}
		case ch == '[' || ch == ']':
			b.WriteRune(ch)
			k = re
		default:
			b.WriteRune(ch)
		}
	}
	p.kind = k
	if k == re {
		b.WriteByte('$')
		r, err := regexp.Compile(b.String())
		if err != nil {
			return err
		}
		p.re = r
	}
	return nil
}

func (p *pattern) match(s string) bool {
	switch p.kind {
	case exact:
		return s == p.text
	case prefix:
		return strings.HasPrefix(s, p.text[:len(p.text)-2])
	case suffix:
		suf := p.text[2:]
		if strings.HasSuffix(s, suf) {
			return true
		}
		return suf != "" && suf[0] == '/' && s == suf[1:]
	default:
		return p.re.MatchString(s)
	}
}

// Match reports whether file (slash-separated, relative to the context
// root) is excluded: the last pattern matching it or one of its parent
// directories decides.
func (m *Matcher) Match(file string) bool {
	if m == nil || len(m.pats) == 0 {
		return false
	}
	file = path.Clean(file)
	if file == "." {
		return false
	}
	parent := path.Dir(file)
	var parents []string
	if parent != "." {
		parents = strings.Split(parent, "/")
	}
	matched := false
	for _, p := range m.pats {
		if p.exclusion != matched {
			continue
		}
		ok := p.match(file)
		if !ok {
			for i := range parents {
				if p.match(strings.Join(parents[:i+1], "/")) {
					ok = true
					break
				}
			}
		}
		if ok {
			matched = !p.exclusion
		}
	}
	return matched
}
