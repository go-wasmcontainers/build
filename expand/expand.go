// Package expand substitutes variables in Containerfile words the way the
// Dockerfile reference's "Environment replacement" section specifies, with
// the grammar of BuildKit's frontend/dockerfile/shell lexer: single and
// double quotes, the escape token, $name and ${name}, and the modifiers
// ${name:-word} ${name-word} ${name:+word} ${name+word} ${name?word}
// ${name:?word} ${name#pattern} ${name##pattern} ${name%pattern}
// ${name%%pattern} ${name/pattern/repl} ${name//pattern/repl}.
//
// Nothing is ever executed: there is no command substitution, no
// arithmetic, no globbing of the file system. A value is inserted as data
// and never re-scanned, so a variable cannot inject syntax. The output and
// the nesting of ${...} are bounded.
package expand

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Env answers variable lookups.
type Env interface {
	Get(name string) (string, bool)
}

// Map is an Env over a map.
type Map map[string]string

// Get implements Env.
func (m Map) Get(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// Limits of one expansion.
const (
	// MaxOutput is the largest result of one word, in bytes.
	MaxOutput = 1 << 20
	// MaxDepth is the deepest nesting of ${...}.
	MaxDepth = 32
)

// ErrTooLong reports an expansion beyond MaxOutput.
var ErrTooLong = errors.New("expansion exceeds 1 MiB")

// Lexer expands words with an escape token ('\\' or '`').
type Lexer struct {
	Escape rune
}

// Word expands s and returns it as one string (quotes removed, spaces
// kept).
func (l Lexer) Word(s string, env Env) (string, error) {
	w := l.newState(s, env)
	res, _, err := w.until(eof, false, false)
	if err != nil {
		return "", fmt.Errorf("failed to process %q: %w", s, err)
	}
	return res, nil
}

// Words expands s and splits the result into words, after substitution,
// honouring quotes: an unquoted value that holds spaces becomes several
// words.
func (l Lexer) Words(s string, env Env) ([]string, error) {
	w := l.newState(s, env)
	_, words, err := w.until(eof, false, false)
	if err != nil {
		return nil, fmt.Errorf("failed to process %q: %w", s, err)
	}
	return words, nil
}

const eof rune = -1

type state struct {
	src   []rune
	pos   int
	env   Env
	esc   rune
	raw   bool // keep escape tokens in the result (pattern operands)
	depth int
}

func (l Lexer) newState(s string, env Env) *state {
	esc := l.Escape
	if esc == 0 {
		esc = '\\'
	}
	if env == nil {
		env = Map(nil)
	}
	return &state{src: []rune(s), env: env, esc: esc}
}

func (s *state) peek() rune {
	if s.pos >= len(s.src) {
		return eof
	}
	return s.src[s.pos]
}

func (s *state) next() rune {
	if s.pos >= len(s.src) {
		return eof
	}
	r := s.src[s.pos]
	s.pos++
	return r
}

// words accumulates word splitting the way the reference lexer does.
type words struct {
	buf    strings.Builder
	list   []string
	inWord bool
}

func (w *words) addChar(r rune) {
	if unicode.IsSpace(r) {
		if w.inWord && w.buf.Len() != 0 {
			w.list = append(w.list, w.buf.String())
			w.buf.Reset()
			w.inWord = false
		}
		return
	}
	w.addRaw(r)
}

func (w *words) addRaw(r rune) {
	w.buf.WriteRune(r)
	w.inWord = true
}

func (w *words) addString(str string) {
	for _, r := range str {
		w.addChar(r)
	}
}

func (w *words) addRawString(str string) {
	w.buf.WriteString(str)
	w.inWord = true
}

func (w *words) get() []string {
	if w.buf.Len() > 0 {
		w.list = append(w.list, w.buf.String())
		w.buf.Reset()
		w.inWord = false
	}
	return w.list
}

// until processes input up to stop (consumed) or the end. raw keeps escape
// tokens; pattern doubles literal escapes inside quotes so the pattern
// compiler sees them.
func (s *state) until(stop rune, raw, pattern bool) (string, []string, error) {
	var res strings.Builder
	var ws words
	saved := s.raw
	s.raw = raw
	defer func() { s.raw = saved }()
	for s.peek() != eof {
		ch := s.peek()
		if stop != eof && ch == stop {
			s.next()
			return res.String(), ws.get(), nil
		}
		switch ch {
		case '$':
			v, err := s.dollar()
			if err != nil {
				return "", nil, err
			}
			res.WriteString(v)
			ws.addString(v)
		case '<':
			v := s.heredocMarker()
			res.WriteString(v)
			ws.addRawString(v)
		case '\'':
			v, err := s.singleQuote(pattern)
			if err != nil {
				return "", nil, err
			}
			res.WriteString(v)
			ws.addRawString(v)
		case '"':
			v, err := s.doubleQuote(pattern)
			if err != nil {
				return "", nil, err
			}
			res.WriteString(v)
			ws.addRawString(v)
		default:
			ch = s.next()
			if ch == s.esc {
				if s.raw {
					ws.addRaw(ch)
					res.WriteRune(ch)
				}
				ch = s.next()
				if ch == eof {
					break
				}
				ws.addRaw(ch)
			} else {
				ws.addChar(ch)
			}
			res.WriteRune(ch)
		}
		if res.Len() > MaxOutput {
			return "", nil, ErrTooLong
		}
	}
	if stop != eof {
		return "", nil, fmt.Errorf("unexpected end of statement while looking for matching %s", string(stop))
	}
	return res.String(), ws.get(), nil
}

// heredocMarker keeps "<<" and the blanks after it together in one word.
func (s *state) heredocMarker() string {
	s.next()
	if s.peek() != '<' {
		return "<"
	}
	s.next()
	var b strings.Builder
	b.WriteString("<<")
	for r := s.peek(); r == ' ' || r == '\t' || r == '\r'; r = s.peek() {
		b.WriteRune(s.next())
	}
	return b.String()
}

func (s *state) singleQuote(pattern bool) (string, error) {
	var b strings.Builder
	s.next()
	for {
		ch := s.next()
		switch ch {
		case eof:
			return "", errors.New("unexpected end of statement while looking for matching single-quote")
		case '\'':
			return b.String(), nil
		}
		if pattern && ch == s.esc {
			b.WriteRune(ch)
		}
		b.WriteRune(ch)
	}
}

func (s *state) doubleQuote(pattern bool) (string, error) {
	var b strings.Builder
	s.next()
	for {
		switch s.peek() {
		case eof:
			return "", errors.New("unexpected end of statement while looking for matching double-quote")
		case '"':
			s.next()
			return b.String(), nil
		case '$':
			v, err := s.dollar()
			if err != nil {
				return "", err
			}
			b.WriteString(v)
		default:
			ch := s.next()
			if ch == s.esc {
				switch s.peek() {
				case eof:
					continue
				case '"', '$', s.esc:
					if s.raw {
						b.WriteRune(ch)
					}
					ch = s.next()
				default:
					if s.raw {
						b.WriteRune(ch)
					}
				}
			}
			b.WriteRune(ch)
		}
		if b.Len() > MaxOutput {
			return "", ErrTooLong
		}
	}
}

func (s *state) lookup(name string) (string, bool) {
	return s.env.Get(name)
}

func (s *state) dollar() (string, error) {
	s.next()
	if s.peek() != '{' {
		name := s.name()
		if name == "" {
			return "$", nil
		}
		v, _ := s.lookup(name)
		return v, nil
	}
	s.depth++
	defer func() { s.depth-- }()
	if s.depth > MaxDepth {
		return "", fmt.Errorf("substitutions nested deeper than %d", MaxDepth)
	}
	s.next()
	switch s.peek() {
	case eof:
		return "", errors.New("syntax error: missing '}'")
	case '{', '}', ':':
		return "", errors.New("syntax error: bad substitution")
	}
	name := s.name()
	ch := s.next()
	mod := string(ch)
	nullIsUnset := false
	switch ch {
	case '}':
		v, _ := s.lookup(name)
		return v, nil
	case ':':
		nullIsUnset = true
		ch = s.next()
		mod += string(ch)
		fallthrough
	case '+', '-', '?', '#', '%':
		pat := ch == '#' || ch == '%'
		if nullIsUnset && pat {
			return "", fmt.Errorf("unsupported modifier (%s) in substitution", mod)
		}
		word, _, err := s.until('}', pat, pat)
		if err != nil {
			if s.peek() == eof {
				return "", errors.New("syntax error: missing '}'")
			}
			return "", err
		}
		value, set := s.lookup(name)
		switch ch {
		case '-':
			if !set || (nullIsUnset && value == "") {
				return word, nil
			}
			return value, nil
		case '+':
			if !set || (nullIsUnset && value == "") {
				return "", nil
			}
			return word, nil
		case '?':
			if !set {
				msg := "is not allowed to be unset"
				if word != "" {
					msg = word
				}
				return "", fmt.Errorf("%s: %s", name, msg)
			}
			if nullIsUnset && value == "" {
				msg := "is not allowed to be empty"
				if word != "" {
					msg = word
				}
				return "", fmt.Errorf("%s: %s", name, msg)
			}
			return value, nil
		case '%', '#':
			greedy := false
			if word != "" && rune(word[0]) == ch {
				greedy = true
				word = word[1:]
			}
			if ch == '%' {
				return trimSuffix(word, value, greedy)
			}
			return trimPrefix(word, value, greedy)
		default:
			return "", fmt.Errorf("unsupported modifier (%s) in substitution", mod)
		}
	case '/':
		all := s.peek() == '/'
		if all {
			s.next()
		}
		pattern, _, err := s.until('/', true, true)
		if err != nil {
			if s.peek() == eof {
				return "", errors.New("syntax error: missing '/' in ${}")
			}
			return "", err
		}
		repl, _, err := s.until('}', true, false)
		if err != nil {
			if s.peek() == eof {
				return "", errors.New("syntax error: missing '}'")
			}
			return "", err
		}
		value, _ := s.lookup(name)
		re, err := patternRegexp(pattern, true, false)
		if err != nil {
			return "", fmt.Errorf("invalid pattern (%s) in substitution: %w", pattern, err)
		}
		if all {
			// Literal replacement: "$1" in repl is data, not a group.
			return re.ReplaceAllLiteralString(value, repl), nil
		}
		if idx := re.FindStringIndex(value); idx != nil {
			value = value[:idx[0]] + repl + value[idx[1]:]
		}
		return value, nil
	default:
		return "", fmt.Errorf("unsupported modifier (%s) in substitution", mod)
	}
}

// name reads a variable name: letters, digits and '_', a run of digits
// (a positional parameter), or one special parameter character.
func (s *state) name() string {
	var b strings.Builder
	for s.peek() != eof {
		ch := s.peek()
		if b.Len() == 0 && unicode.IsDigit(ch) {
			for s.peek() != eof && unicode.IsDigit(s.peek()) {
				b.WriteRune(s.next())
			}
			return b.String()
		}
		if b.Len() == 0 && isSpecialParam(ch) {
			return string(s.next())
		}
		if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && ch != '_' {
			break
		}
		b.WriteRune(s.next())
	}
	return b.String()
}

func isSpecialParam(r rune) bool {
	switch r {
	case '@', '*', '#', '?', '-', '$', '!', '0':
		return true
	}
	return false
}

// patternRegexp compiles a shell pattern ('?' one character, '*' any run,
// '\' escaping '*', '?' and '\'; no bracket expressions) to a regular
// expression. Go's regexp runs in linear time, so no pattern can make a
// substitution slow.
func patternRegexp(pattern string, greedy, anchored bool) (*regexp.Regexp, error) {
	var b strings.Builder
	if anchored {
		b.WriteByte('^')
	}
	star := ".*?"
	if greedy {
		star = ".*"
	}
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '*':
			b.WriteString(star)
			continue
		case '?':
			b.WriteByte('.')
			continue
		case '\\':
			if i+1 < len(rs) && (rs[i+1] == '}' || rs[i+1] == '/') {
				continue
			}
			i++
			if i >= len(rs) {
				return nil, errors.New("invalid escape at end of pattern")
			}
			r = rs[i]
			if r != '*' && r != '?' && r != '\\' {
				return nil, fmt.Errorf("invalid escape '\\%c'", r)
			}
			b.WriteByte('\\')
		default:
			if strings.ContainsRune(`[]{}.+()|^$`, r) {
				b.WriteByte('\\')
			}
		}
		b.WriteRune(r)
	}
	return regexp.Compile(b.String())
}

func trimPrefix(pattern, value string, greedy bool) (string, error) {
	re, err := patternRegexp(pattern, greedy, true)
	if err != nil {
		return "", fmt.Errorf("invalid pattern (%s) in substitution: %w", pattern, err)
	}
	if idx := re.FindStringIndex(value); idx != nil {
		value = value[idx[1]:]
	}
	return value, nil
}

// trimSuffix reverses both strings so the shortest or longest match from
// the end becomes a leftmost one.
func trimSuffix(pattern, value string, greedy bool) (string, error) {
	out, err := trimPrefix(reversePattern(pattern), reverse(value), greedy)
	if err != nil {
		return "", err
	}
	return reverse(out), nil
}

func reversePattern(p string) string {
	rs := []rune(p)
	out := make([]rune, len(rs))
	last := len(rs) - 1
	for i := 0; i <= last; {
		if rs[i] == '\\' && i != last {
			out[last-i-1] = rs[i]
			out[last-i] = rs[i+1]
			i += 2
			continue
		}
		out[last-i] = rs[i]
		i++
	}
	return string(out)
}

func reverse(s string) string {
	rs := []rune(s)
	for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
		rs[i], rs[j] = rs[j], rs[i]
	}
	return string(rs)
}
