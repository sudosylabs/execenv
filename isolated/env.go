package isolated

import (
	"context"
	"io"
	"sync"

	"github.com/sudosylabs/execenv"
)

type environment struct {
	mu      sync.Mutex
	id      execenv.ID
	image   Image
	network execenv.Network
	host    *Host
	inst    instance
	treeDir string
	frozen  bool
	revoked bool
	term    *terminal
	files   map[string]node
	obs     *observation
	watchErr error
}

func (e *environment) ID() execenv.ID { return e.id }

func (e *environment) Attach(ctx context.Context, win execenv.Window) (execenv.Terminal, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("attach", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("attach"); err != nil {
		return nil, err
	}
	if e.term != nil && !e.term.finished() {
		return nil, execenv.Error("attach", execenv.ErrBusy)
	}
	term := newTerminal(win)
	e.term = term
	return term, nil
}

// Freeze stops PTY and tree I/O and pauses the microVM. The instance is
// kept so Thaw does not boot a new machine.
func (e *environment) Freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("freeze", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revoked {
		return execenv.Error("freeze", execenv.ErrRevoked)
	}
	e.frozen = true
	if e.term != nil {
		e.term.kill(execenv.ErrFrozen)
		e.term = nil
	}
	e.failWatch(execenv.ErrFrozen)
	if e.inst != nil {
		_ = e.inst.Pause()
	}
	return nil
}

func (e *environment) Thaw(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("thaw", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revoked {
		return execenv.Error("thaw", execenv.ErrRevoked)
	}
	e.frozen = false
	if e.inst != nil {
		_ = e.inst.Resume()
	}
	return nil
}

func (e *environment) Revoke(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("revoke", err)
	}
	return e.host.Revoke(ctx, e.id)
}

func (e *environment) revoke() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked = true
	e.frozen = false
	if e.term != nil {
		e.term.kill(execenv.ErrRevoked)
		e.term = nil
	}
	e.failWatch(execenv.ErrRevoked)
	if e.inst != nil {
		_ = e.inst.Stop()
		e.inst = nil
	}
	return nil
}

func (e *environment) guard(op string) error {
	if e.revoked {
		return execenv.Error(op, execenv.ErrRevoked)
	}
	if e.frozen {
		return execenv.Error(op, execenv.ErrFrozen)
	}
	return nil
}

type terminal struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	win    execenv.Window
	closed bool
	dead   error
}

func newTerminal(win execenv.Window) *terminal {
	if win.Cols == 0 {
		win.Cols = 80
	}
	if win.Rows == 0 {
		win.Rows = 24
	}
	term := &terminal{win: win}
	term.cond = sync.NewCond(&term.mu)
	return term
}

func (t *terminal) finished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed || t.dead != nil
}

func (t *terminal) kill(reason error) {
	t.mu.Lock()
	t.dead = reason
	t.cond.Broadcast()
	t.mu.Unlock()
}

func (t *terminal) Read(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for len(t.buf) == 0 && !t.closed && t.dead == nil {
		t.cond.Wait()
	}
	if t.dead != nil {
		return 0, execenv.Error("read", t.dead)
	}
	if len(t.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

func (t *terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dead != nil {
		return 0, execenv.Error("write", t.dead)
	}
	if t.closed {
		return 0, execenv.Error("write", execenv.ErrClosed)
	}
	t.buf = append(t.buf, p...)
	t.cond.Broadcast()
	return len(p), nil
}

func (t *terminal) Resize(ctx context.Context, win execenv.Window) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("resize", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dead != nil {
		return execenv.Error("resize", t.dead)
	}
	if t.closed {
		return execenv.Error("resize", execenv.ErrClosed)
	}
	if win.Cols == 0 {
		win.Cols = 80
	}
	if win.Rows == 0 {
		win.Rows = 24
	}
	t.win = win
	return nil
}

func (t *terminal) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.cond.Broadcast()
	t.mu.Unlock()
	return nil
}
