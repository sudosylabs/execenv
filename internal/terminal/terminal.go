// Package terminal is the host-side terminal session every adapter shares:
// one shell, a bounded output history, at most one attached client, a
// detach grace period, takeover, tail replay and redraw.
//
// An adapter supplies the raw shell stream and a way to start one. The
// session pumps shell output into a ring continuously, so a slow or absent
// client never blocks the shell. Grant callers never import this package.
package terminal

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
)

// Shell is one raw shell stream. Read returns the shell's output and an
// error (io.EOF for a normal exit) once the shell is gone. Close hangs the
// shell up. The session owns the Shell once Start returns it.
type Shell interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(ctx context.Context, win execenv.Window) error
	Close() error
}

// Starter starts a shell sized to win.
type Starter func(ctx context.Context, win execenv.Window) (Shell, error)

// Config describes one environment's terminal.
type Config struct {
	Start Starter
	// Grace is how long a detached shell keeps running. Zero uses
	// execenv.DefaultTerminalGrace.
	Grace time.Duration
	// History and Tail override execenv.TerminalHistoryBytes and
	// execenv.TerminalTailBytes (tests only).
	History int
	Tail    int
	// AfterFunc schedules grace expiry. Nil uses time.AfterFunc. The
	// returned function cancels the call.
	AfterFunc func(d time.Duration, f func()) (stop func() bool)
}

// Manager owns one environment's terminal session.
type Manager struct {
	cfg     Config
	attachQ chan struct{}

	mu     sync.Mutex
	sess   *session
	frozen bool
	ended  error
}

type session struct {
	m     *Manager
	id    execenv.TerminalID
	shell Shell
	// writeQ serializes input with takeover so no input from a replaced
	// attachment lands after the new one is live.
	writeQ chan struct{}

	// Guarded by m.mu.
	ring      []byte
	total     uint64
	done      bool
	doneErr   error
	att       *attachment
	cond      *sync.Cond
	stopGrace func() bool
	graceGen  uint64
}

type attachment struct {
	s      *session
	start  uint64
	offset uint64
	err    error // non-nil once this attachment is no longer live
}

var _ execenv.Terminal = (*attachment)(nil)

// New returns a Manager with no session.
func New(cfg Config) *Manager {
	if cfg.Grace <= 0 {
		cfg.Grace = execenv.DefaultTerminalGrace
	}
	if cfg.History <= 0 {
		cfg.History = execenv.TerminalHistoryBytes
	}
	if cfg.Tail <= 0 {
		cfg.Tail = execenv.TerminalTailBytes
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	return &Manager{cfg: cfg, attachQ: make(chan struct{}, 1)}
}

func normalize(win execenv.Window) execenv.Window {
	if win.Cols == 0 {
		win.Cols = 80
	}
	if win.Rows == 0 {
		win.Rows = 24
	}
	return win
}

func acquire(ctx context.Context, q chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case q <- struct{}{}:
		return nil
	}
}

// Attach implements execenv.Env.Attach for one environment.
func (m *Manager) Attach(ctx context.Context, req execenv.AttachRequest) (execenv.Terminal, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("attach", err)
	}
	if req.Resume != nil && req.Resume.Terminal == "" {
		return nil, execenv.Error("attach", execenv.ErrInvalid)
	}
	win := normalize(req.Window)
	if err := acquire(ctx, m.attachQ); err != nil {
		return nil, execenv.Error("attach", err)
	}
	defer func() { <-m.attachQ }()
	for {
		m.mu.Lock()
		s, err := m.admitLocked(req)
		m.mu.Unlock()
		if err != nil {
			return nil, execenv.Error("attach", err)
		}
		if s == nil {
			return m.start(ctx, win)
		}
		term, retry, err := m.join(ctx, s, req, win)
		if retry {
			continue
		}
		if err != nil {
			return nil, execenv.Error("attach", err)
		}
		return term, nil
	}
}

// admitLocked validates req against the current state. A nil session with
// no error means a new shell must start.
func (m *Manager) admitLocked(req execenv.AttachRequest) (*session, error) {
	if m.ended != nil {
		return nil, m.ended
	}
	if m.frozen {
		return nil, execenv.ErrFrozen
	}
	s := m.sess
	if req.Resume != nil {
		if s == nil || s.id != req.Resume.Terminal {
			return nil, execenv.ErrNotFound
		}
		if req.Resume.Offset > s.total {
			return nil, execenv.ErrInvalid
		}
	}
	if s != nil && s.att != nil && !req.Takeover {
		return nil, execenv.ErrBusy
	}
	return s, nil
}

func (m *Manager) start(ctx context.Context, win execenv.Window) (execenv.Terminal, error) {
	shell, err := m.cfg.Start(ctx, win)
	if err != nil {
		// The adapter's Starter names its own operation.
		return nil, err
	}
	s := &session{
		m:      m,
		id:     execenv.TerminalID(rand.Text()),
		shell:  shell,
		writeQ: make(chan struct{}, 1),
		ring:   make([]byte, m.cfg.History),
	}
	s.cond = sync.NewCond(&m.mu)
	m.mu.Lock()
	if err := m.admitStartLocked(); err != nil {
		m.mu.Unlock()
		_ = shell.Close()
		return nil, execenv.Error("attach", err)
	}
	a := &attachment{s: s}
	s.att = a
	m.sess = s
	m.mu.Unlock()
	go s.pump()
	return a, nil
}

// admitStartLocked rechecks state after the shell started without the lock.
func (m *Manager) admitStartLocked() error {
	if m.ended != nil {
		return m.ended
	}
	if m.frozen {
		return execenv.ErrFrozen
	}
	if m.sess != nil {
		return execenv.ErrBusy
	}
	return nil
}

// join attaches to the live session s. retry means s ended while waiting
// for its input queue, and admission must run again.
func (m *Manager) join(ctx context.Context, s *session, req execenv.AttachRequest, win execenv.Window) (execenv.Terminal, bool, error) {
	if err := acquire(ctx, s.writeQ); err != nil {
		return nil, false, err
	}
	defer func() { <-s.writeQ }()
	m.mu.Lock()
	if m.sess != s {
		m.mu.Unlock()
		return nil, true, nil
	}
	if _, err := m.admitLocked(req); err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	m.mu.Unlock()
	// Window applies on every attach. A plain join forces full-screen
	// programs to redraw by changing the size and changing it back.
	if req.Resume == nil {
		nudge := win
		if nudge.Cols > 1 {
			nudge.Cols--
		} else {
			nudge.Cols++
		}
		if err := s.shell.Resize(ctx, nudge); err != nil {
			return nil, false, err
		}
	}
	if err := s.shell.Resize(ctx, win); err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sess != s {
		return nil, true, nil
	}
	if _, err := m.admitLocked(req); err != nil {
		return nil, false, err
	}
	first := s.first()
	var start uint64
	if req.Resume != nil {
		start = max(req.Resume.Offset, first)
	} else {
		start = first
		if s.total > uint64(m.cfg.Tail) {
			start = max(start, s.total-uint64(m.cfg.Tail))
		}
	}
	if s.att != nil {
		s.att.err = execenv.ErrSuperseded
	}
	s.cancelGraceLocked()
	a := &attachment{s: s, start: start, offset: start}
	s.att = a
	s.cond.Broadcast()
	return a, false, nil
}

// Live reports whether a shell is running, attached or not.
func (m *Manager) Live() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sess != nil
}

// Freeze stops input, resize and attach, and pauses grace expiry. Output
// already retained stays readable.
func (m *Manager) Freeze() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frozen = true
	if s := m.sess; s != nil {
		s.cancelGraceLocked()
	}
}

// Thaw reopens the session. A detached session's grace restarts.
func (m *Manager) Thaw() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.frozen = false
	if s := m.sess; s != nil && s.att == nil {
		s.armGraceLocked()
	}
}

// End hangs the session up for good with reason (revoke). The attached
// Read and Write return reason, and every later Attach fails with it.
func (m *Manager) End(reason error) {
	m.mu.Lock()
	if m.ended == nil {
		m.ended = reason
	}
	s := m.sess
	m.sess = nil
	if s != nil {
		s.finishLocked(reason)
		if s.att != nil {
			s.att.err = reason
		}
	}
	m.mu.Unlock()
	if s != nil {
		_ = s.shell.Close()
	}
}

func (s *session) first() uint64 {
	if s.total > uint64(len(s.ring)) {
		return s.total - uint64(len(s.ring))
	}
	return 0
}

// pump moves shell output into the ring until the shell is gone.
func (s *session) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.shell.Read(buf)
		if n > 0 {
			s.m.mu.Lock()
			s.appendLocked(buf[:n])
			s.cond.Broadcast()
			s.m.mu.Unlock()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.EOF
			}
			s.m.mu.Lock()
			live := !s.done
			if live {
				s.finishLocked(err)
				if s.m.sess == s {
					s.m.sess = nil
				}
			}
			s.m.mu.Unlock()
			if live {
				_ = s.shell.Close()
			}
			return
		}
	}
}

func (s *session) appendLocked(p []byte) {
	size := uint64(len(s.ring))
	if uint64(len(p)) > size {
		s.total += uint64(len(p)) - size
		p = p[len(p)-int(size):]
	}
	for len(p) > 0 {
		at := s.total % size
		n := copy(s.ring[at:], p)
		p = p[n:]
		s.total += uint64(n)
	}
}

// finishLocked marks the shell gone. Readers drain what remains, then see
// reason.
func (s *session) finishLocked(reason error) {
	if s.done {
		return
	}
	s.done = true
	s.doneErr = reason
	s.cancelGraceLocked()
	s.cond.Broadcast()
}

func (s *session) armGraceLocked() {
	s.cancelGraceLocked()
	if s.done || s.m.frozen {
		return
	}
	s.graceGen++
	gen := s.graceGen
	s.stopGrace = s.m.cfg.AfterFunc(s.m.cfg.Grace, func() { s.expire(gen) })
}

func (s *session) cancelGraceLocked() {
	s.graceGen++
	if s.stopGrace != nil {
		s.stopGrace()
		s.stopGrace = nil
	}
}

func (s *session) expire(gen uint64) {
	m := s.m
	m.mu.Lock()
	if gen != s.graceGen || m.sess != s || s.att != nil || m.frozen || s.done {
		m.mu.Unlock()
		return
	}
	s.stopGrace = nil
	s.finishLocked(execenv.ErrClosed)
	m.sess = nil
	m.mu.Unlock()
	_ = s.shell.Close()
}

func (a *attachment) ID() execenv.TerminalID { return a.s.id }

func (a *attachment) Start() uint64 { return a.start }

func (a *attachment) Offset() uint64 {
	a.s.m.mu.Lock()
	defer a.s.m.mu.Unlock()
	return a.offset
}

// Read blocks for output. A reader that fell behind the retained history
// skips ahead to its oldest byte; Offset shows the jump.
func (a *attachment) Read(p []byte) (int, error) {
	s := a.s
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	for {
		if a.err != nil {
			return 0, execenv.Error("read", a.err)
		}
		if first := s.first(); a.offset < first {
			a.offset = first
		}
		if a.offset < s.total {
			if len(p) == 0 {
				return 0, nil
			}
			size := uint64(len(s.ring))
			at := a.offset % size
			end := min(size, at+(s.total-a.offset))
			n := copy(p, s.ring[at:end])
			a.offset += uint64(n)
			return n, nil
		}
		if s.done {
			if s.doneErr == io.EOF {
				return 0, io.EOF
			}
			return 0, execenv.Error("read", s.doneErr)
		}
		s.cond.Wait()
	}
}

// liveLocked reports why input cannot go to the shell, if it cannot.
func (a *attachment) liveLocked() error {
	if a.err != nil {
		return a.err
	}
	if a.s.m.frozen {
		return execenv.ErrFrozen
	}
	if a.s.done {
		return execenv.ErrClosed
	}
	return nil
}

func (a *attachment) Write(p []byte) (int, error) {
	s := a.s
	s.writeQ <- struct{}{}
	defer func() { <-s.writeQ }()
	s.m.mu.Lock()
	err := a.liveLocked()
	s.m.mu.Unlock()
	if err != nil {
		return 0, execenv.Error("write", err)
	}
	n, err := s.shell.Write(p)
	if err != nil {
		return n, execenv.Error("write", err)
	}
	return n, nil
}

func (a *attachment) Resize(ctx context.Context, win execenv.Window) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("resize", err)
	}
	s := a.s
	s.m.mu.Lock()
	err := a.liveLocked()
	s.m.mu.Unlock()
	if err != nil {
		return execenv.Error("resize", err)
	}
	if err := s.shell.Resize(ctx, normalize(win)); err != nil {
		return execenv.Error("resize", err)
	}
	return nil
}

// Close detaches. The session keeps running for the grace period.
func (a *attachment) Close() error {
	s := a.s
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	if a.err != nil {
		return nil
	}
	a.err = execenv.ErrClosed
	if s.att == a {
		s.att = nil
		if s.m.sess == s {
			s.armGraceLocked()
		}
	}
	s.cond.Broadcast()
	return nil
}

// Hangup ends the shell now.
func (a *attachment) Hangup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("hangup", err)
	}
	s := a.s
	m := s.m
	m.mu.Lock()
	if err := a.liveLocked(); err != nil && !(s.done && a.err == nil) {
		m.mu.Unlock()
		return execenv.Error("hangup", err)
	}
	a.err = execenv.ErrClosed
	live := !s.done
	s.finishLocked(execenv.ErrClosed)
	if s.att == a {
		s.att = nil
	}
	if m.sess == s {
		m.sess = nil
	}
	m.mu.Unlock()
	if live {
		_ = s.shell.Close()
	}
	return nil
}
