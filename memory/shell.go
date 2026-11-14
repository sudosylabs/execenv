package memory

import (
	"context"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/sudosylabs/execenv"
)

// shell is the in-memory stand-in for a login shell behind a PTY. It echoes
// input like a terminal in canonical mode and runs each line. It knows only
// what tests need: NAME=value, echo with $NAME expansion and quote removal,
// seq [FIRST] LAST, true, and exit. A command is found by its last path
// element, so /bin/true is true. Anything else prints "not found".
//
// It records itself in the process log as /bin/sh -l, and every command
// line it runs as a start and exit pair: exit 0, or 127 when not found.
// Unlike a real shell it records builtins too. Assignments and exit are not
// commands; exit ends the shell with status 0, and a hangup ends it by
// SIGHUP.
type shell struct {
	mu     sync.Mutex
	cond   *sync.Cond
	out    []byte
	line   []byte
	vars   map[string]string
	win    execenv.Window
	exited bool
	closed bool
	log    *processLog
	pid    int
}

func newShell(win execenv.Window, log *processLog) *shell {
	s := &shell{vars: make(map[string]string), win: win, log: log}
	s.cond = sync.NewCond(&s.mu)
	s.pid = log.start(1, []string{"/bin/sh", "-l"})
	return s
}

func (s *shell) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.out) == 0 && !s.exited && !s.closed {
		s.cond.Wait()
	}
	if len(s.out) > 0 {
		n := copy(p, s.out)
		s.out = s.out[n:]
		return n, nil
	}
	return 0, io.EOF
}

func (s *shell) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.exited {
		return 0, execenv.ErrClosed
	}
	for _, c := range p {
		if s.exited {
			break
		}
		if c != '\r' && c != '\n' {
			s.out = append(s.out, c)
			s.line = append(s.line, c)
			continue
		}
		s.out = append(s.out, '\r', '\n')
		line := string(s.line)
		s.line = s.line[:0]
		s.run(line)
	}
	s.cond.Broadcast()
	return len(p), nil
}

func (s *shell) Resize(ctx context.Context, win execenv.Window) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.exited {
		return execenv.ErrClosed
	}
	s.win = win
	return nil
}

func (s *shell) Close() error {
	s.mu.Lock()
	if !s.closed && !s.exited {
		s.log.killed(s.pid, 1)
	}
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	return nil
}

func (s *shell) run(line string) {
	words := strings.Fields(line)
	if len(words) == 0 {
		return
	}
	if name, value, ok := strings.Cut(words[0], "="); ok && len(words) == 1 && validName(name) {
		s.vars[name] = s.expand(value)
		return
	}
	if words[0] == "exit" {
		s.exited = true
		s.log.exit(s.pid, 0)
		return
	}
	argv := make([]string, 0, len(words))
	for _, word := range words {
		argv = append(argv, s.expand(word))
	}
	pid := s.log.start(s.pid, argv)
	code := 0
	switch path.Base(words[0]) {
	case "true":
	case "echo":
		expanded := make([]string, 0, len(words)-1)
		for _, word := range words[1:] {
			expanded = append(expanded, s.expand(word))
		}
		s.print(strings.Join(expanded, " "))
	case "seq":
		s.seq(words[1:])
	default:
		s.print("sh: " + words[0] + ": not found")
		code = 127
	}
	s.log.exit(pid, code)
}

func (s *shell) seq(args []string) {
	first, last := 1, 0
	var err error
	switch len(args) {
	case 1:
		last, err = strconv.Atoi(args[0])
	case 2:
		first, err = strconv.Atoi(args[0])
		if err == nil {
			last, err = strconv.Atoi(args[1])
		}
	default:
		err = strconv.ErrSyntax
	}
	if err != nil {
		s.print("seq: invalid argument")
		return
	}
	for i := first; i <= last; i++ {
		s.out = strconv.AppendInt(s.out, int64(i), 10)
		s.out = append(s.out, '\r', '\n')
	}
}

func (s *shell) print(text string) {
	s.out = append(s.out, text...)
	s.out = append(s.out, '\r', '\n')
}

// expand removes quotes and substitutes $NAME.
func (s *shell) expand(word string) string {
	var b strings.Builder
	for i := 0; i < len(word); i++ {
		c := word[i]
		switch {
		case c == '"' || c == '\'':
		case c == '$':
			j := i + 1
			for j < len(word) && nameByte(word[j], j == i+1) {
				j++
			}
			if j == i+1 {
				b.WriteByte(c)
				continue
			}
			b.WriteString(s.vars[word[i+1:j]])
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !nameByte(name[i], i == 0) {
			return false
		}
	}
	return true
}

func nameByte(c byte, first bool) bool {
	switch {
	case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		return true
	case c >= '0' && c <= '9':
		return !first
	default:
		return false
	}
}
