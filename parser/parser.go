// Package parser reads a Containerfile (a Dockerfile: same syntax) into a
// list of instructions, following the grammar of the Dockerfile reference
// and of BuildKit's frontend/dockerfile/parser: parser directives
// (syntax, escape, check) before anything else, comments, line
// continuation with the escape token, and per-instruction argument forms
// (JSON array or shell string, whitespace lists, key=value pairs,
// ONBUILD sub-instructions, HEALTHCHECK types). It does no variable
// expansion: that is package expand's, at build time.
//
// BuildKit's parser itself is not imported: it would add 34 packages and
// 6.1 MB to a js/wasm build (measured in docs/REFERENCES.md), and the
// builder runs in the browser playground too.
//
// The input is untrusted: its size, line length and instruction count are
// bounded.
package parser

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits bound what Parse accepts.
type Limits struct {
	MaxBytes        int64 // whole file
	MaxLineBytes    int   // one physical line
	MaxInstructions int
}

// DefaultLimits are generous for any real Containerfile.
var DefaultLimits = Limits{MaxBytes: 1 << 20, MaxLineBytes: 256 << 10, MaxInstructions: 4096}

// Node is one instruction.
type Node struct {
	// Cmd is the instruction, lowercased ("from", "run", ...).
	Cmd string
	// Original is the logical line as written (continuations joined).
	Original string
	// Flags are the leading --name[=value] words, unexpanded.
	Flags []string
	// Args are the arguments, in BuildKit's AST order: a JSON array's
	// elements; a shell-form string as one element; whitespace-separated
	// words; for ENV and LABEL, triplets key, value, separator ("=" or ""
	// for the legacy "ENV key value" form); for ARG, the raw words; for
	// HEALTHCHECK, the type then its command.
	Args []string
	// JSON reports the exec (JSON array) form.
	JSON bool
	// Sub is ONBUILD's instruction.
	Sub *Node
	// StartLine and EndLine are 1-based.
	StartLine, EndLine int
}

// Result is a parsed file.
type Result struct {
	Nodes []*Node
	// Escape is the escape token: '\\' or '`'.
	Escape rune
	// Directives found at the top ("syntax", "escape", "check").
	Directives map[string]string
	// Warnings are non-fatal findings (empty continuation lines).
	Warnings []string
}

// Error locates a parse error.
type Error struct {
	Line int
	Err  error
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %v", e.Line, e.Err)
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// ErrTooLarge reports input beyond the limits.
var ErrTooLarge = errors.New("containerfile too large")

var (
	errNotStringArray = errors.New("when using JSON array syntax, arrays must be comprised of strings only")
	errNotJSONArray   = errors.New("not a JSON array")
)

type directives struct {
	escape rune
	done   bool
	seen   map[string]bool
	values map[string]string
}

// parseLine looks for a parser directive while they are still allowed.
func (d *directives) parseLine(line []byte) (bool, error) {
	if d.done {
		return false, nil
	}
	rest, ok := bytes.CutPrefix(line, []byte("#"))
	if !ok {
		d.done = true
		return false, nil
	}
	rest = bytes.TrimLeftFunc(rest, unicode.IsSpace)
	// ^([a-zA-Z][a-zA-Z0-9]*)\s*=\s*(.+?)\s*$
	i := 0
	for i < len(rest) && (isASCIILetter(rest[i]) || (i > 0 && rest[i] >= '0' && rest[i] <= '9')) {
		i++
	}
	if i == 0 {
		d.done = true
		return false, nil
	}
	key := strings.ToLower(string(rest[:i]))
	after := bytes.TrimLeftFunc(rest[i:], unicode.IsSpace)
	val, ok := bytes.CutPrefix(after, []byte("="))
	if !ok {
		d.done = true
		return false, nil
	}
	v := strings.TrimSpace(string(val))
	if v == "" {
		d.done = true
		return false, nil
	}
	switch key {
	case "syntax", "escape", "check":
	default:
		d.done = true
		return false, nil
	}
	if d.seen[key] {
		return false, fmt.Errorf("only one %s parser directive can be used", key)
	}
	d.seen[key] = true
	d.values[key] = v
	if key == "escape" {
		if v != "`" && v != `\` {
			return false, fmt.Errorf("invalid escape token '%s' does not match ` or \\", v)
		}
		d.escape = rune(v[0])
	}
	return true, nil
}

func isASCIILetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

func isComment(line []byte) bool {
	line = bytes.TrimLeftFunc(line, unicode.IsSpace)
	return len(line) > 0 && line[0] == '#'
}

func trimNewline(b []byte) []byte { return bytes.TrimRight(b, "\r\n") }

// continuation reports whether line ends with the escape token (after
// trailing blanks) that is not itself escaped, and strips it.
func continuation(line []byte, esc rune) ([]byte, bool) {
	t := bytes.TrimRight(line, " \t")
	if len(t) == 0 || rune(t[len(t)-1]) != esc {
		return line, false
	}
	if len(t) >= 2 && rune(t[len(t)-2]) == esc {
		return line, false
	}
	return t[:len(t)-1], true
}

// Parse reads a Containerfile.
func Parse(r io.Reader, lim Limits) (*Result, error) {
	if lim.MaxBytes <= 0 {
		lim = DefaultLimits
	}
	data, err := io.ReadAll(io.LimitReader(r, lim.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > lim.MaxBytes {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, lim.MaxBytes)
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(data) {
		return nil, errors.New("containerfile is not valid UTF-8")
	}
	d := &directives{escape: '\\', seen: map[string]bool{}, values: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(data))
	// The token limit is the larger of max and the buffer's capacity.
	sc.Buffer(make([]byte, 0, min(4096, lim.MaxLineBytes)), lim.MaxLineBytes)
	sc.Split(scanLines)
	res := &Result{}
	line := 0
	var buf bytes.Buffer
	for sc.Scan() {
		raw := sc.Bytes()
		cur := bytes.TrimLeftFunc(trimNewline(raw), unicode.IsSpace)
		if _, err := d.parseLine(cur); err != nil {
			return nil, &Error{Line: line + 1, Err: err}
		}
		if isComment(cur) {
			cur = nil
		}
		line++
		start := line
		cur, more := continuation(cur, d.escape)
		if !more && len(cur) == 0 {
			continue
		}
		buf.Reset()
		buf.Write(cur)
		emptyCont := false
		for more && sc.Scan() {
			nb := trimNewline(sc.Bytes())
			if _, err := d.parseLine(nb); err != nil {
				return nil, &Error{Line: line + 1, Err: err}
			}
			line++
			if isComment(nb) {
				continue
			}
			if len(bytes.TrimLeftFunc(nb, unicode.IsSpace)) == 0 {
				emptyCont = true
				continue
			}
			nb, more = continuation(nb, d.escape)
			buf.Write(nb)
			if buf.Len() > int(lim.MaxBytes) {
				return nil, &Error{Line: line, Err: ErrTooLarge}
			}
		}
		text := buf.String()
		if emptyCont {
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: empty continuation line found in: %s", line, text))
		}
		n, err := nodeFromLine(text, d.escape, false)
		if err != nil {
			return nil, &Error{Line: start, Err: err}
		}
		n.StartLine, n.EndLine = start, line
		if err := checkHeredoc(n); err != nil {
			return nil, &Error{Line: start, Err: err}
		}
		res.Nodes = append(res.Nodes, n)
		if len(res.Nodes) > lim.MaxInstructions {
			return nil, &Error{Line: start, Err: fmt.Errorf("%w: more than %d instructions", ErrTooLarge, lim.MaxInstructions)}
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, &Error{Line: line + 1, Err: fmt.Errorf("%w: a line is longer than %d bytes", ErrTooLarge, lim.MaxLineBytes)}
		}
		return nil, err
	}
	if len(res.Nodes) == 0 {
		return nil, &Error{Line: line, Err: errors.New("file with no instructions")}
	}
	res.Escape = d.escape
	res.Directives = d.values
	return res, nil
}

// scanLines keeps line endings, as the reference parser does.
func scanLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// Instructions this parser knows, with their argument forms.
var known = map[string]func(rest string, esc rune) ([]string, bool, error){
	"add":         maybeJSONToList,
	"arg":         nameOrNameVal,
	"cmd":         maybeJSON,
	"copy":        maybeJSONToList,
	"entrypoint":  maybeJSON,
	"env":         nameVal("ENV"),
	"expose":      whitespaceList,
	"from":        whitespaceList,
	"label":       nameVal("LABEL"),
	"maintainer":  str,
	"run":         maybeJSON,
	"shell":       maybeJSON,
	"stopsignal":  str,
	"user":        str,
	"volume":      maybeJSONToList,
	"workdir":     str,
	"healthcheck": healthcheck,
}

func nodeFromLine(line string, esc rune, sub bool) (*Node, error) {
	cmd, flags, rest, err := splitCommand(line, esc)
	if err != nil {
		return nil, err
	}
	n := &Node{Cmd: strings.ToLower(cmd), Original: line, Flags: flags}
	if n.Cmd == "onbuild" {
		if sub {
			return nil, errors.New("chaining ONBUILD via `ONBUILD ONBUILD` isn't allowed")
		}
		if rest == "" {
			return n, nil
		}
		child, err := nodeFromLine(rest, esc, true)
		if err != nil {
			return nil, err
		}
		if child.Cmd == "from" || child.Cmd == "maintainer" {
			return nil, fmt.Errorf("%s isn't allowed as an ONBUILD trigger", strings.ToUpper(child.Cmd))
		}
		n.Sub = child
		return n, nil
	}
	fn, ok := known[n.Cmd]
	if !ok {
		return nil, fmt.Errorf("unknown instruction: %s", strings.ToUpper(cmd))
	}
	n.Args, n.JSON, err = fn(rest, esc)
	return n, err
}

func isBlank(r rune) bool { return r == ' ' || r == '\t' || r == '\v' || r == '\f' || r == '\r' }

// splitWhitespace splits on runs of [\t\v\f\r ], at most n parts (n < 0:
// all), like the reference's reWhitespace.Split.
func splitWhitespace(s string, n int) []string {
	var out []string
	for {
		if n > 0 && len(out) == n-1 {
			return append(out, s)
		}
		i := strings.IndexFunc(s, isBlank)
		if i < 0 {
			return append(out, s)
		}
		out = append(out, s[:i])
		j := i
		for j < len(s) && isBlank(rune(s[j])) {
			j++
		}
		s = s[j:]
	}
}

func splitCommand(line string, esc rune) (string, []string, string, error) {
	parts := splitWhitespace(strings.TrimSpace(line), 2)
	var rest string
	var flags []string
	if len(parts) == 2 {
		rest, flags = extractFlags(parts[1], esc)
	}
	return parts[0], flags, strings.TrimSpace(rest), nil
}

// extractFlags takes the leading "--" words off a line.
func extractFlags(line string, esc rune) (string, []string) {
	const (
		inSpaces = iota
		inWord
		inQuote
	)
	words := []string{}
	phase := inSpaces
	var b strings.Builder
	quote := rune(0)
	blankOK := false
	var ch rune
	for pos := 0; pos <= len(line); pos++ {
		if pos != len(line) {
			ch = rune(line[pos])
		}
		if phase == inSpaces {
			if pos == len(line) {
				break
			}
			if unicode.IsSpace(ch) {
				continue
			}
			if ch != '-' || pos+1 == len(line) || line[pos+1] != '-' {
				return line[pos:], words
			}
			phase = inWord
		}
		if (phase == inWord || phase == inQuote) && pos == len(line) {
			if w := b.String(); w != "--" && (blankOK || len(w) > 0) {
				words = append(words, w)
			}
			break
		}
		if phase == inWord {
			if unicode.IsSpace(ch) {
				w := b.String()
				phase = inSpaces
				if w == "--" {
					return line[pos:], words
				}
				if blankOK || len(w) > 0 {
					words = append(words, w)
				}
				b.Reset()
				blankOK = false
				continue
			}
			if ch == '\'' || ch == '"' {
				quote = ch
				blankOK = true
				phase = inQuote
				continue
			}
			if ch == esc {
				if pos+1 == len(line) {
					continue
				}
				pos++
				ch = rune(line[pos])
			}
			b.WriteByte(byte(ch))
			continue
		}
		if phase == inQuote {
			if ch == quote {
				phase = inWord
				continue
			}
			if ch == esc {
				if pos+1 == len(line) {
					phase = inWord
					continue
				}
				pos++
				ch = rune(line[pos])
			}
			b.WriteByte(byte(ch))
		}
	}
	return "", words
}

// parseWords splits on spaces, keeping quotes and escapes for the
// expander to process later.
func parseWords(rest string, esc rune) []string {
	const (
		inSpaces = iota
		inWord
		inQuote
	)
	words := []string{}
	phase := inSpaces
	quote := rune(0)
	blankOK := false
	var b strings.Builder
	var ch rune
	var w int
	for pos := 0; pos <= len(rest); pos += w {
		if pos != len(rest) {
			ch, w = utf8.DecodeRuneInString(rest[pos:])
		}
		if phase == inSpaces {
			if pos == len(rest) {
				break
			}
			if unicode.IsSpace(ch) {
				continue
			}
			phase = inWord
		}
		if (phase == inWord || phase == inQuote) && pos == len(rest) {
			if blankOK || b.Len() > 0 {
				words = append(words, b.String())
			}
			break
		}
		if phase == inWord {
			if unicode.IsSpace(ch) {
				phase = inSpaces
				if blankOK || b.Len() > 0 {
					words = append(words, b.String())
				}
				b.Reset()
				blankOK = false
				continue
			}
			if ch == '\'' || ch == '"' {
				quote = ch
				blankOK = true
				phase = inQuote
			}
			if ch == esc {
				if pos+w == len(rest) {
					continue
				}
				b.WriteRune(ch)
				pos += w
				ch, w = utf8.DecodeRuneInString(rest[pos:])
			}
			b.WriteRune(ch)
			continue
		}
		if phase == inQuote {
			if ch == quote {
				phase = inWord
			}
			if ch == esc && quote != '\'' {
				if pos+w == len(rest) {
					phase = inWord
					continue
				}
				pos += w
				b.WriteRune(ch)
				ch, w = utf8.DecodeRuneInString(rest[pos:])
			}
			b.WriteRune(ch)
		}
	}
	return words
}

func nameVal(key string) func(string, rune) ([]string, bool, error) {
	return func(rest string, esc rune) ([]string, bool, error) {
		words := parseWords(rest, esc)
		if len(words) == 0 {
			return nil, false, nil
		}
		if !strings.Contains(words[0], "=") {
			parts := splitWhitespace(rest, 2)
			if len(parts) < 2 {
				return nil, false, fmt.Errorf("%s must have two arguments", key)
			}
			return []string{parts[0], parts[1], ""}, false, nil
		}
		var out []string
		for _, w := range words {
			k, v, ok := strings.Cut(w, "=")
			if !ok {
				return nil, false, fmt.Errorf("syntax error - can't find = in %q. Must be of the form: name=value", w)
			}
			out = append(out, k, v, "=")
		}
		return out, false, nil
	}
}

func nameOrNameVal(rest string, esc rune) ([]string, bool, error) {
	w := parseWords(rest, esc)
	if len(w) == 0 {
		return nil, false, nil
	}
	return w, false, nil
}

func whitespaceList(rest string, _ rune) ([]string, bool, error) {
	if rest == "" {
		return nil, false, nil
	}
	return splitWhitespace(rest, -1), false, nil
}

func str(rest string, _ rune) ([]string, bool, error) {
	if rest == "" {
		return nil, false, nil
	}
	return []string{rest}, false, nil
}

func parseJSON(rest string) ([]string, error) {
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if !strings.HasPrefix(rest, "[") {
		return nil, errNotJSONArray
	}
	var arr []any
	if err := json.Unmarshal([]byte(rest), &arr); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			return nil, errNotStringArray
		}
		out = append(out, s)
	}
	return out, nil
}

func maybeJSON(rest string, _ rune) ([]string, bool, error) {
	if rest == "" {
		return nil, false, nil
	}
	a, err := parseJSON(rest)
	if err == nil {
		return a, true, nil
	}
	if errors.Is(err, errNotStringArray) {
		return nil, false, err
	}
	return []string{rest}, false, nil
}

func maybeJSONToList(rest string, esc rune) ([]string, bool, error) {
	a, err := parseJSON(rest)
	if err == nil {
		return a, true, nil
	}
	if errors.Is(err, errNotStringArray) {
		return nil, false, err
	}
	return whitespaceList(rest, esc)
}

func healthcheck(rest string, esc rune) ([]string, bool, error) {
	sep := strings.IndexFunc(rest, unicode.IsSpace)
	if sep < 0 {
		sep = len(rest)
	}
	if sep == 0 {
		return nil, false, nil
	}
	typ := rest[:sep]
	cmd, isJSON, err := maybeJSON(strings.TrimLeftFunc(rest[sep:], unicode.IsSpace), esc)
	if err != nil {
		return nil, false, err
	}
	return append([]string{typ}, cmd...), isJSON, nil
}

// checkHeredoc refuses here-documents (RUN/COPY/ADD <<EOF): not
// supported yet, and silently treating "<<EOF" as a word would build
// something else than what was written.
func checkHeredoc(n *Node) error {
	c := n
	if c.Cmd == "onbuild" && c.Sub != nil {
		c = c.Sub
	}
	if (c.Cmd != "run" && c.Cmd != "copy" && c.Cmd != "add") || c.JSON {
		return nil
	}
	for _, a := range c.Args {
		for _, w := range parseWords(a, '\\') {
			i := 0
			for i < len(w) && w[i] >= '0' && w[i] <= '9' {
				i++
			}
			if !strings.HasPrefix(w[i:], "<<") {
				continue
			}
			name := strings.TrimLeft(strings.TrimPrefix(w[i+2:], "-"), " \t")
			if name != "" && !strings.Contains(name, "<") {
				return fmt.Errorf("here-documents (%s) are not supported yet", w)
			}
		}
	}
	return nil
}

// Dump renders a node as BuildKit's parser test results do, so its golden
// files apply unchanged.
func (n *Node) Dump() string {
	var b strings.Builder
	b.WriteString(n.Cmd)
	if len(n.Flags) > 0 {
		fmt.Fprintf(&b, " %q", n.Flags)
	}
	if n.Sub != nil {
		b.WriteString(" (" + n.Sub.Dump() + ")")
	}
	for _, a := range n.Args {
		b.WriteString(" " + strconv.Quote(a))
	}
	return strings.TrimSpace(b.String())
}

// Dump renders a whole file, one "(instruction ...)" per line.
func (r *Result) Dump() string {
	var b strings.Builder
	for _, n := range r.Nodes {
		b.WriteString("(" + n.Dump() + ")\n")
	}
	return b.String()
}
