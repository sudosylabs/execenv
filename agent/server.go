package agent

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/mux"
	"github.com/sudosylabs/execenv/internal/tree"
	"github.com/sudosylabs/execenv/internal/watchlog"
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
		projected: make(tree.Snapshot),
		sess:      newSession(conn),
		log:       watchlog.New(watchlog.DefaultCapacity),
	}
	defer s.shutdown()
	go func() {
		<-ctx.Done()
		_ = s.sess.Close()
	}()
	for {
		f, err := s.sess.Recv()
		if err != nil {
			return nil
		}
		switch f.Kind {
		case kindPty:
			s.writePty(f)
		case kindRequest:
			s.handle(ctx, f)
		}
	}
}

type server struct {
	home string
	sess *mux.Session

	mu sync.Mutex
	// projected is the last caller ReplaceTree/Apply, used only for
	// version-skip. Open and Watch read Home on disk, including guest files.
	projected   tree.Snapshot
	frozen      bool
	term        *shell
	termStream  uint64
	watching    bool
	watchStream uint64
	watchErr    error
	snap        map[string]fileMeta
	pollStop    chan struct{}
	log         *watchlog.Log
}

func (s *server) shutdown() {
	s.mu.Lock()
	s.killShell()
	s.stopPoll(execenv.ErrClosed)
	s.mu.Unlock()
	_ = s.sess.Close()
}

func (s *server) handle(ctx context.Context, f frame) {
	if f.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(0, f.Deadline))
		defer cancel()
	}
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
		extra, err = s.watch(ctx, f)
	case methodUnwatch:
		err = s.unwatch(f)
	case methodAttach:
		err = s.attach(ctx, f)
	case methodDetach:
		err = s.detach(f)
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
	return s.sess.Send(frame{
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
	next, err := tree.Replace(s.projected, args.Tree)
	if err != nil {
		return err
	}
	// Full snapshot: unlisted paths, including guest-created files, go.
	if err := rewriteHome(s.home, next); err != nil {
		return err
	}
	s.projected = next
	s.invalidateWatch(execenv.ErrLagged)
	s.log.Reset()
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
	next, err := tree.Apply(s.projected, args.Batch)
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

func (s *server) watch(ctx context.Context, f frame) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var args watchArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return nil, execenv.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(); err != nil {
		return nil, err
	}
	if s.watching {
		return nil, execenv.ErrBusy
	}
	cursor, replay, err := s.log.Since(args.After)
	if err != nil {
		return nil, err
	}
	s.watching = true
	s.watchStream = args.Stream
	s.watchErr = nil
	if s.pollStop == nil {
		s.resetSnap()
		s.pollStop = make(chan struct{})
		go s.poll(s.pollStop)
	}
	for _, event := range replay {
		raw, err := encodeExtra(event)
		if err != nil {
			s.watching = false
			return nil, execenv.ErrUnavailable
		}
		if err := s.sess.Send(frame{Kind: kindWatch, Stream: args.Stream, Extra: raw}); err != nil {
			s.watching = false
			return nil, execenv.ErrClosed
		}
	}
	return encodeExtra(watchResult{Cursor: cursor})
}

func (s *server) unwatch(f frame) error {
	var args streamArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watchStream == args.Stream {
		s.watching = false
		s.watchStream = 0
		s.watchErr = execenv.ErrClosed
	}
	return nil
}

func (s *server) attach(ctx context.Context, f frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var args attachArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
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
	s.termStream = args.Stream
	go s.copyPty(args.Stream, sh)
	return nil
}

func (s *server) copyPty(stream uint64, sh *shell) {
	buf := make([]byte, 32*1024)
	for {
		n, err := sh.master.Read(buf)
		if n > 0 {
			_ = s.sess.Send(frame{
				Kind:   kindPty,
				Stream: stream,
				Extra:  append([]byte(nil), buf[:n]...),
			})
		}
		if err != nil {
			s.mu.Lock()
			frozen := s.frozen
			if s.term == sh {
				s.term = nil
				s.termStream = 0
			}
			s.mu.Unlock()
			_ = s.sess.Send(frame{Kind: kindPty, Stream: stream, Status: ptyEndStatus(frozen, err)})
			return
		}
	}
}

func ptyEndStatus(frozen bool, err error) string {
	switch {
	case frozen:
		return statusOf(execenv.ErrFrozen)
	case err == nil || err == io.EOF || isPtyHangup(err):
		return ""
	default:
		return statusOf(err)
	}
}

func (s *server) writePty(f frame) {
	s.mu.Lock()
	term := s.term
	stream := s.termStream
	frozen := s.frozen
	s.mu.Unlock()
	if term == nil || frozen || stream != f.Stream {
		return
	}
	_, _ = term.master.Write(f.Extra)
}

func (s *server) detach(f frame) error {
	var args streamArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.termStream == args.Stream {
		s.killShell()
	}
	return nil
}

func (s *server) resize(ctx context.Context, f frame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var args resizeArgs
	if err := decodeExtra(f.Extra, &args); err != nil || args.Stream == 0 {
		return execenv.ErrInvalid
	}
	s.mu.Lock()
	term := s.term
	stream := s.termStream
	frozen := s.frozen
	s.mu.Unlock()
	if frozen {
		return execenv.ErrFrozen
	}
	if term == nil || stream != args.Stream {
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
	s.termStream = 0
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
