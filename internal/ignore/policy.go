package ignore

import (
	"bytes"
	"path"
	"strings"

	"github.com/sudosylabs/execenv"
)

// MaxFileBytes bounds one read of the ignore file. A larger file does not
// parse.
const MaxFileBytes = execenv.MaxIgnoreFileBytes

// Rules is one environment's ignore configuration: the workspace-relative
// ignore file and the fallback used when that file is missing or does not
// parse. It re-parses the file only when its bytes change. A nil Rules
// observes everything. The owning environment serializes access.
type Rules struct {
	file     string
	fallback *Matcher
	body     []byte
	parsed   *Matcher
	cached   bool
}

// New compiles rules that execenv.ValidateIgnoreRules accepts.
func New(file string, fallback []string) (*Rules, error) {
	if execenv.ValidateIgnoreRules(execenv.IgnoreRules{File: file, Fallback: fallback}) != nil {
		return nil, ErrPattern
	}
	m, err := Compile(fallback)
	if err != nil {
		return nil, err
	}
	return &Rules{file: file, fallback: m}, nil
}

// File returns the ignore file path, or "" for nil rules.
func (r *Rules) File() string {
	if r == nil {
		return ""
	}
	return r.file
}

// Policy returns the decision for the ignore file's current body. present
// is false when the file is missing or is not a regular file.
func (r *Rules) Policy(body []byte, present bool) Policy {
	if r == nil {
		return Policy{}
	}
	if !present || len(body) > MaxFileBytes {
		return Policy{file: r.file, matcher: r.fallback}
	}
	if !r.cached || !bytes.Equal(r.body, body) {
		m, err := Parse(body)
		if err != nil {
			m = nil
		}
		r.body, r.parsed, r.cached = bytes.Clone(body), m, true
	}
	if r.parsed == nil {
		return Policy{file: r.file, matcher: r.fallback}
	}
	return Policy{file: r.file, matcher: r.parsed}
}

// Policy is one capture's decision. The zero value observes everything.
type Policy struct {
	file    string
	matcher *Matcher
}

// Active reports whether this policy can suppress any path.
func (p Policy) Active() bool { return p.matcher != nil && len(p.matcher.patterns) > 0 }

// Observed reports whether name is reported. A held path (one the
// projection holds or the stream already reports) is always observed, and
// so are the ignore file and its ancestor directories. The caller applies
// this top down and never descends into a directory it does not observe.
func (p Policy) Observed(name string, dir, held bool) bool {
	if held || !p.Active() || name == p.file || strings.HasPrefix(p.file, name+"/") {
		return true
	}
	return !p.matcher.Ignored(name, dir)
}

// Keep applies the policy top down to a whole tree, given each path's
// directory bit. A path is kept only when its parent is kept.
func (p Policy) Keep(nodes map[string]bool, held func(string) bool) map[string]bool {
	kept := make(map[string]bool, len(nodes))
	var visit func(string) bool
	decided := make(map[string]bool, len(nodes))
	visit = func(name string) bool {
		if keep, ok := decided[name]; ok {
			return keep
		}
		keep := true
		if parent := path.Dir(name); parent != "." {
			if _, exists := nodes[parent]; exists {
				keep = visit(parent)
			}
		}
		keep = keep && p.Observed(name, nodes[name], held(name))
		decided[name] = keep
		return keep
	}
	for name := range nodes {
		if visit(name) {
			kept[name] = nodes[name]
		}
	}
	return kept
}
