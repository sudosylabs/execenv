package agent

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"

	"github.com/sudosylabs/execenv"
)

// Config is one agent's occupancy of a home directory.
type Config struct {
	// Home is the projected tree. It is cwd and $HOME for the login
	// shell. The image root is not this directory and stays read-only.
	Home string
}

// Serve handles one host connection. Closing the PTY or the connection is
// hangup: Home stays. The caller tears the machine down separately.
func Serve(ctx context.Context, conn net.Conn, cfg Config) error {
	if cfg.Home == "" {
		return execenv.Error("agent", execenv.ErrInvalid)
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return execenv.Error("agent", err)
	}
	s := &server{
		home:      cfg.Home,
		projected: make(map[string]node),
		sess:      newSession(conn),
	}
	defer s.shutdown()
	go func() {
		<-ctx.Done()
		_ = s.sess.close()
	}()
	for {
		f, err := s.sess.recv()
		if err != nil {
			return nil
		}
		switch f.Kind {
		case kindPty:
			s.writePty(f.Extra)
		case kindRequest:
			s.handle(ctx, f)
		}
	}
}

type server struct {
	home string
	sess *session

	mu sync.Mutex
	// projected is the last caller ReplaceTree/Apply, used only for
	// version-skip. Open and Watch read Home on disk, including guest files.
	projected map[string]node
	frozen    bool
	term      *shell
	watching  bool
	watchErr  error
	snap      map[string]fileMeta
	pollStop  chan struct{}
}

func (s *server) shutdown() {
	s.mu.Lock()
	s.killShell()
	s.stopPoll(execenv.ErrClosed)
	s.mu.Unlock()
	_ = s.sess.close()
}

func (s *server) handle(ctx context.Context, f frame) {
	var extra []byte
	var err error
	switch f.Method {
	case methodReplace:
		err = s.replace(ctx, f)
	case methodApply:
		err = s.apply(ctx, f)
	case methodOpen:
		extra, err = s.open(ctx, f)
	case methodWatch:
		err = s.watch(ctx)
	case methodUnwatch:
		err = s.unwatch()
	case methodAttach:
		err = s.attach(ctx, f)
	case methodDetach:
		err = s.detach()
	case methodResize:
		err = s.resize(ctx, f)
	case methodFreeze:
		err = s.freeze(ctx)
	case methodThaw:
		err = s.thaw(ctx)
	default:
		err = execenv.ErrInvalid
	}
	_ = s.reply(f, err, extra)
}

func (s *server) reply(req frame, err error, extra []byte) error {
	return s.sess.send(frame{
		Seq:    req.Seq,
		Kind:   kindResponse,
		Method: req.Method,
		Status: statusOf(err),
		Extra:  extra,
	})
}

func (s *server) guard() error {
	if s.frozen {
		return execenv.ErrFrozen
	}
	return nil
}

func (s *server) replace(ctx context.Context, f frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var args replaceArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(); err != nil {
		return err
	}
	next, err := replaceInto(s.projected, args.Tree)
	if err != nil {
		return err
	}
	// Full snapshot: unlisted paths, including guest-created files, go.
	if err := rewriteHome(s.home, next); err != nil {
		return err
	}
	s.projected = next
	s.resetSnap()
	return nil
}

func (s *server) apply(ctx context.Context, f frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var args applyArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(); err != nil {
		return err
	}
	next, err := applyInto(s.projected, args.Batch)
	if err != nil {
		return err
	}
	// Incremental: guest files that the batch does not name stay on disk.
	if err := applyHome(s.home, args.Batch); err != nil {
		return err
	}
	s.projected = next
	s.resetSnap()
	return nil
}

func (s *server) open(ctx context.Context, f frame) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var args openArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return nil, execenv.ErrInvalid
	}
	if err := execenv.ValidatePath(args.Path); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(); err != nil {
		return nil, err
	}
	full, err := execenv.ResolvePath(s.home, args.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(full)
	if err != nil || !info.Mode().IsRegular() {
		return nil, execenv.ErrNotFound
	}
	if info.Size() > execenv.MaxFileBytes {
		return nil, execenv.ErrTooLarge
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, execenv.ErrNotFound
	}
	return encodeExtra(data)
}

func (s *server) watch(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(); err != nil {
		return err
	}
	if s.watching {
		return execenv.ErrBusy
	}
	s.watching = true
	s.watchErr = nil
	s.resetSnap()
	s.pollStop = make(chan struct{})
	go s.poll(s.pollStop)
	return nil
}

func (s *server) unwatch() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopPoll(execenv.ErrClosed)
	return nil
}

func (s *server) attach(ctx context.Context, f frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var args attachArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(); err != nil {
		return err
	}
	if s.term != nil && !s.term.done() {
		return execenv.ErrBusy
	}
	sh, err := startShell(s.home, args.Window)
	if err != nil {
		return execenv.ErrUnavailable
	}
	s.term = sh
	go s.copyPty(sh)
	return nil
}

func (s *server) copyPty(sh *shell) {
	buf := make([]byte, 32*1024)
	for {
		n, err := sh.master.Read(buf)
		if n > 0 {
			_ = s.sess.send(frame{
				Kind:  kindPty,
				Extra: append([]byte(nil), buf[:n]...),
			})
		}
		if err != nil {
			status := ""
			if err != io.EOF {
				status = statusOf(err)
			}
			_ = s.sess.send(frame{Kind: kindPty, Status: status})
			s.mu.Lock()
			if s.term == sh {
				s.term = nil
			}
			s.mu.Unlock()
			return
		}
	}
}

func (s *server) writePty(p []byte) {
	s.mu.Lock()
	term := s.term
	frozen := s.frozen
	s.mu.Unlock()
	if term == nil || frozen {
		return
	}
	_, _ = term.master.Write(p)
}

func (s *server) detach() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killShell()
	return nil
}

func (s *server) resize(ctx context.Context, f frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var args resizeArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	s.mu.Lock()
	term := s.term
	frozen := s.frozen
	s.mu.Unlock()
	if frozen {
		return execenv.ErrFrozen
	}
	if term == nil {
		return execenv.ErrClosed
	}
	return setWindow(term.master, args.Window)
}

func (s *server) freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	s.killShell()
	s.stopPoll(execenv.ErrFrozen)
	return nil
}

func (s *server) thaw(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = false
	return nil
}

func (s *server) killShell() {
	if s.term == nil {
		return
	}
	s.term.close()
	s.term = nil
}

type shell struct {
	master *os.File
	cmd    *exec.Cmd
}

func (sh *shell) done() bool {
	if sh == nil || sh.cmd.ProcessState != nil {
		return true
	}
	return false
}

func (sh *shell) close() {
	if sh.cmd.Process != nil {
		hangup(sh.cmd)
	}
	_ = sh.master.Close()
}
