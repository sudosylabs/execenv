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
	"github.com/sudosylabs/execenv/internal/workload"
)

// Config is one agent's occupancy of a projected tree.
type Config struct {
	// Home is the projected tree and the login shell's working directory.
	// The image root is not this directory and stays read-only.
	Home string
	// Scratch is the login shell's $HOME, outside the projected tree, so
	// tool caches and virtual environments are neither observed nor
	// harvested. Empty uses Home (local development only).
	Scratch string
	// ContainWorkload requires a dedicated guest with a usable kernel write
	// barrier and launches every workload process unprivileged. Unsupported
	// configurations fail before serving or accepting a terminal. It is how
	// a real guest serves the journal.
	ContainWorkload bool
	// Barrier serves the journal without a contained workload. It must stop
	// every writer under Home while its function runs. Only tests whose
	// files are quiescent may supply one; a guest uses ContainWorkload.
	// Without either, the helper advertises no journal and every host
	// refuses it.
	Barrier WriteBarrier
}

// WriteBarrier stops every writer under Home while fn runs. An error from
// fn means the disk state is uncertain; the barrier stays closed after it.
type WriteBarrier interface {
	Stopped(ctx context.Context, fn func() error) error
}

// Serve handles one host connection. Closing the PTY or the connection is
// hangup: Home stays. The caller tears the machine down separately.
func Serve(ctx context.Context, conn net.Conn, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.Home == "" {
		return execenv.Error("agent", execenv.ErrInvalid)
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return execenv.Error("agent", err)
	}
	scratch := cfg.Scratch
	if scratch == "" {
		scratch = cfg.Home
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return execenv.Error("agent", err)
	}
	var group *workload.Group
	if cfg.ContainWorkload {
		var err error
		group, err = workload.Open()
		if err != nil {
			return execenv.Error("workload", err)
		}
		defer group.Close()
		if err := group.OwnHome(cfg.Home); err != nil {
			return execenv.Error("workload", err)
		}
		if err := group.OwnHome(scratch); err != nil {
			return execenv.Error("workload", err)
		}
	}
	s := &server{
		workload: group,
		home:     cfg.Home,
		scratch:  scratch,
		sess:     newSession(conn),
	}
	switch {
	case group != nil:
		s.journal = newDiskJournal(cfg.Home, group, group.OwnHome)
	case cfg.Barrier != nil:
		s.journal = newDiskJournal(cfg.Home, cfg.Barrier, nil)
	}
	if s.journal != nil {
		defer s.journal.close()
	}
	defer s.shutdown()
	go func() {
		<-ctx.Done()
		_ = s.sess.Close()
	}()
	return s.serveFrames(ctx)
}

func (s *server) serveFrames(ctx context.Context) error {
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
	home          string
	scratch       string
	workload      *workload.Group
	workloadErr   error
	journal       *diskJournal
	journalUpload *journalUpload
	sess          *mux.Session

	mu         sync.Mutex
	frozen     bool
	term       *shell
	termStream uint64
}

func (s *server) shutdown() {
	s.mu.Lock()
	s.killShell()
	s.mu.Unlock()
	_ = s.sess.Close()
}

func (s *server) handle(ctx context.Context, f frame) {
	if f.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(0, f.Deadline))
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		_ = s.reply(f, err, nil)
		return
	}
	var extra []byte
	var err error
	switch f.Method {
	case methodJournalUploadBegin, methodJournalUploadChunk, methodJournalUploadFinish, methodJournalUploadAbort, methodJournalInitialize, methodJournalApply, methodJournalObserve, methodJournalContent, methodJournalAcknowledge, methodJournalConfirm:
		extra, err = s.journalCall(ctx, f)
	case methodHello:
		extra, err = encodeExtra(helloResult{Version: protocolVersion, Journal: s.journal != nil})
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
	if s.workload != nil {
		if err := s.workload.Check(); err != nil {
			return err
		}
	}
	if s.workloadErr != nil {
		return execenv.ErrUnavailable
	}
	if s.frozen {
		return execenv.ErrFrozen
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
	if s.workload != nil {
		if err := s.workload.Kill(ctx); err != nil {
			s.workloadErr = err
			return err
		}
	}
	sh, err := startShell(s.home, s.scratch, args.Window, s.workload)
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
	return nil
}

func (s *server) thaw(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workload != nil {
		if err := s.workload.Check(); err != nil {
			return err
		}
	}
	s.frozen = false
	return nil
}

func (s *server) killShell() {
	if s.term == nil {
		return
	}
	s.term.close()
	if s.workload != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.workloadErr = s.workload.Kill(ctx)
		cancel()
	}
	s.term = nil
	s.termStream = 0
}

type shell struct {
	master *os.File
	cmd    *exec.Cmd
	exited chan struct{}
}

func (sh *shell) done() bool {
	if sh == nil {
		return true
	}
	select {
	case <-sh.exited:
		return true
	default:
		return false
	}
}

func (sh *shell) close() {
	if sh.cmd.Process != nil {
		hangup(sh.cmd, sh.exited)
	}
	_ = sh.master.Close()
}
