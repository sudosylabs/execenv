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
// element, so /bin/true is true. Anything else prints "not found". A
// command's output can be redirected to a workspace file with "> path" or
// appended with ">> path"; the file is written as the shell's started
// process, so observations name its tag.
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
	// command is set when the shell stands for one command a Start ran:
	// the log has that command as the shell's own start, so running it
	// logs nothing more.
	command bool
	// terminal echoes input and ends lines with CRLF, as a PTY does; a
	// command does neither.
	terminal bool
	// code is the last command's exit code, and the shell's on exit.
	code int
	// signal is what ended the shell when Close did, if anything.
	signal int
	// write writes a redirected command's output to a workspace path.
	write func(path string, data []byte, appending bool) error
	// redirecting is set while a command's output goes to a file: lines
	// then end with LF even on a terminal.
	redirecting bool
}

// newShell starts a fake shell under tag. With argv it stands for that one
// command, which the log records as the tagged process, as a guest's does.
func newShell(win execenv.Window, log *processLog, terminal bool, tag string, argv []string, write func(path string, data []byte, appending bool) error) *shell {
	s := &shell{vars: make(map[string]string), win: win, log: log, terminal: terminal, command: len(argv) > 0, write: write}
	s.cond = sync.NewCond(&s.mu)
	started := []string{"/bin/sh", "-l"}
	if len(argv) > 0 {
		started = argv
	}
	s.pid = log.start(1, started, tag)
	return s
}

func (s *shell) newline() string {
	if s.terminal && !s.redirecting {
		return "\r\n"
	}
	return "\n"
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
			if s.terminal {
				s.out = append(s.out, c)
			}
			s.line = append(s.line, c)
			continue
		}
		if s.terminal {
			s.out = append(s.out, '\r', '\n')
		}
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
		if len(words) > 1 {
			if code, err := strconv.Atoi(words[1]); err == nil {
				s.code = code
			}
		}
		s.exited = true
		s.log.exit(s.pid, s.code)
		return
	}
	words, target, appending, ok := redirection(words)
	if !ok {
		s.print("sh: syntax error")
		s.code = 2
		return
	}
	if target != "" {
		before := len(s.out)
		s.redirecting = true
		defer func() {
			s.redirecting = false
			data := append([]byte(nil), s.out[before:]...)
			s.out = s.out[:before]
			if err := s.write(s.expand(target), data, appending); err != nil {
				s.print("sh: " + target + ": cannot create")
				s.code = 1
			}
		}()
	}
	if len(words) == 0 {
		return
	}
	argv := make([]string, 0, len(words))
	for _, word := range words {
		argv = append(argv, s.expand(word))
	}
	pid := 0
	if s.command {
		// The command is the shell's own process: it is logged already.
		s.command = false
	} else {
		pid = s.log.start(s.pid, argv, "")
	}
	code := 0
	switch path.Base(words[0]) {
	case "true":
	case "false":
		code = 1
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
	if pid != 0 {
		s.log.exit(pid, code)
	}
	s.code = code
}

// redirection takes one "> path" or ">> path", spaced or not, out of
// words. ok is false when an operator names no path.
func redirection(words []string) (rest []string, target string, appending, ok bool) {
	rest = make([]string, 0, len(words))
	for i := 0; i < len(words); i++ {
		word := words[i]
		if !strings.HasPrefix(word, ">") {
			rest = append(rest, word)
			continue
		}
		appending = strings.HasPrefix(word, ">>")
		target = strings.TrimLeft(word, ">")
		if target == "" {
			if i+1 >= len(words) {
				return nil, "", false, false
			}
			i++
			target = words[i]
		}
	}
	return rest, target, appending, true
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
		s.out = append(s.out, s.newline()...)
	}
}

func (s *shell) print(text string) {
	s.out = append(s.out, text...)
	s.out = append(s.out, s.newline()...)
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
