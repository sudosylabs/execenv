// Package ignore decides which guest paths an environment leaves out of its
// observation stream. It is shared by every adapter so the gitignore
// matcher and the held-path rule cannot diverge.
//
// A Policy is evaluated top down: an entry is observed when its parent is
// observed and Observed reports true for it. A directory that is not
// observed is never descended into.
package ignore

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrPattern reports an ignore body or pattern that does not parse.
var ErrPattern = errors.New("ignore: malformed pattern")

// Matcher is an ordered gitignore pattern list. The zero value ignores
// nothing.
type Matcher struct{ patterns []pattern }

type pattern struct {
	negate   bool
	dirOnly  bool
	anchored bool
	segments []string
}

// Parse compiles a gitignore body. Bodies with NUL bytes, invalid UTF-8 or
// a malformed pattern do not parse.
func Parse(body []byte) (*Matcher, error) {
	if bytes.IndexByte(body, 0) >= 0 || !utf8.Valid(body) {
		return nil, ErrPattern
	}
	return Compile(strings.Split(string(body), "\n"))
}

// Compile compiles pattern lines in order. Blank and comment lines are
// accepted and ignored.
func Compile(lines []string) (*Matcher, error) {
	m := &Matcher{}
	for _, line := range lines {
		p, ok, err := compile(line)
		if err != nil {
			return nil, err
		}
		if ok {
			m.patterns = append(m.patterns, p)
		}
	}
	return m, nil
}

func compile(line string) (pattern, bool, error) {
	if strings.ContainsAny(line, "\x00\n") || !utf8.ValidString(line) {
		return pattern{}, false, ErrPattern
	}
	line = strings.TrimSuffix(line, "\r")
	line = trimTrailingSpaces(line)
	if line == "" || line[0] == '#' {
		return pattern{}, false, nil
	}
	var p pattern
	if line[0] == '!' {
		p.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") && !escaped(line, len(line)-1) {
		p.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if strings.Contains(line, "/") {
		p.anchored = true
		line = strings.TrimPrefix(line, "/")
	}
	if line == "" {
		// "/" and "!" alone match nothing, as in git.
		return pattern{}, false, nil
	}
	for _, segment := range strings.Split(line, "/") {
		if segment == "" {
			return pattern{}, false, ErrPattern
		}
		if segment != "**" {
			if err := validSegment(segment); err != nil {
				return pattern{}, false, err
			}
		}
		p.segments = append(p.segments, segment)
	}
	return p, true, nil
}

// trimTrailingSpaces drops trailing spaces unless they are escaped.
func trimTrailingSpaces(line string) string {
	end := len(line)
	for end > 0 && line[end-1] == ' ' && !escaped(line, end-1) {
		end--
	}
	return line[:end]
}

// escaped reports whether the byte at index is preceded by an odd number of
// backslashes.
func escaped(s string, index int) bool {
	n := 0
	for i := index - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// validSegment refuses a trailing escape and an unterminated or empty
// bracket expression, so a typo cannot silently widen or narrow a rule.
func validSegment(segment string) error {
	for i := 0; i < len(segment); i++ {
		switch segment[i] {
		case '\\':
			if i+1 >= len(segment) {
				return ErrPattern
			}
			i++
		case '[':
			end, ok := classEnd(segment, i)
			if !ok {
				return ErrPattern
			}
			i = end
		}
	}
	return nil
}

// classEnd returns the index of the ']' closing the class opened at start.
func classEnd(s string, start int) (int, bool) {
	i := start + 1
	if i < len(s) && (s[i] == '!' || s[i] == '^') {
		i++
	}
	first := true
	for ; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			if i+1 >= len(s) {
				return 0, false
			}
			i++
		case s[i] == ']' && !first:
			return i, true
		}
		first = false
	}
	return 0, false
}

// Ignored reports whether name is ignored, including because an ancestor
// directory is: as in git, a path under an excluded directory cannot be
// re-included. dir reports whether name itself is a directory.
func (m *Matcher) Ignored(name string, dir bool) bool {
	if m == nil || len(m.patterns) == 0 {
		return false
	}
	segments := strings.Split(name, "/")
	for i := 1; i < len(segments); i++ {
		if m.decide(segments[:i], true) {
			return true
		}
	}
	return m.decide(segments, dir)
}

// decide applies the last matching pattern to exactly this path.
func (m *Matcher) decide(segments []string, dir bool) bool {
	ignored := false
	for _, p := range m.patterns {
		if p.dirOnly && !dir {
			continue
		}
		if p.matches(segments) {
			ignored = !p.negate
		}
	}
	return ignored
}

func (p pattern) matches(segments []string) bool {
	if !p.anchored {
		return matchSegment(p.segments[0], segments[len(segments)-1])
	}
	return matchSegments(p.segments, segments)
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			if len(rest) == 0 {
				// A trailing "/**" matches everything inside, never the
				// directory itself.
				return len(name) > 0
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 || !matchSegment(pattern[0], name[0]) {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// matchSegment matches one path segment against a validated glob segment:
// '*' any run, '?' one character, '[...]' a class, '\' an escape.
func matchSegment(pattern, name string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchSegment(pattern, name[i:]) {
					return true
				}
				if i < len(name) {
					_, size := utf8.DecodeRuneInString(name[i:])
					i += size - 1
				}
			}
			return false
		case '?':
			if name == "" {
				return false
			}
			_, size := utf8.DecodeRuneInString(name)
			pattern, name = pattern[1:], name[size:]
		case '[':
			if name == "" {
				return false
			}
			end, _ := classEnd(pattern, 0)
			r, size := utf8.DecodeRuneInString(name)
			if !matchClass(pattern[1:end], r) {
				return false
			}
			pattern, name = pattern[end+1:], name[size:]
		default:
			literal := pattern[0]
			if literal == '\\' {
				pattern = pattern[1:]
				literal = pattern[0]
			}
			if name == "" || name[0] != literal {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		}
	}
	return name == ""
}

func matchClass(class string, r rune) bool {
	negate := false
	if class != "" && (class[0] == '!' || class[0] == '^') {
		negate = true
		class = class[1:]
	}
	matched := false
	for class != "" {
		low, size := classRune(class)
		class = class[size:]
		high := low
		if len(class) >= 2 && class[0] == '-' {
			high, size = classRune(class[1:])
			class = class[1+size:]
		}
		if low <= r && r <= high {
			matched = true
		}
	}
	return matched != negate
}

func classRune(class string) (rune, int) {
	if class[0] == '\\' && len(class) > 1 {
		r, size := utf8.DecodeRuneInString(class[1:])
		return r, size + 1
	}
	return utf8.DecodeRuneInString(class)
}
