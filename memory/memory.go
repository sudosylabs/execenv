// Package memory is an in-process execenv host for tests and local development.
package memory

import (
	"context"
	"io"
	"slices"
	"sync"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/control"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/observations"
	"github.com/sudosylabs/execenv/internal/projection"
	"github.com/sudosylabs/execenv/internal/tree"
	"github.com/sudosylabs/execenv/internal/watchlog"
)

var (
	_ execenv.Host     = (*Host)(nil)
	_ execenv.Env      = (*environment)(nil)
	_ execenv.Terminal = (*terminal)(nil)
)

// Config constructs an in-memory host.
type Config struct {
	Images   []execenv.Image
	Slots    int
	Unusable bool
}

// Host is an in-process execenv.Host. It is not isolated.
type Host struct {
	mu       sync.Mutex
	images   []execenv.Image
	slots    int
	unusable bool
	grants   map[execenv.ID]*environment
}

type environment struct {
	control         *control.State
	projection      *projection.State
	semantic        *observations.Log
	guestTree       *guesttree.Snapshot
	projectionFence execenv.ProjectionFence
	mu              sync.Mutex
	id              execenv.ID
	image           execenv.Image
	network         execenv.Network
	host            *Host
	frozen          bool
	revoked         bool
	term            *terminal
	// files is the current projection. Paths are already-validated.
	files tree.Snapshot
	obs   *observation
	log   *watchlog.Log
}

type terminal struct {
	env    *environment
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	win    execenv.Window
	closed bool
	frozen bool
	dead   error
}

// New returns an inert in-memory host.
func New(cfg Config) (*Host, error) {
	slots := cfg.Slots
	if slots <= 0 {
		slots = 1
	}
	return &Host{
		images:   append([]execenv.Image(nil), cfg.Images...),
		slots:    slots,
		unusable: cfg.Unusable,
		grants:   make(map[execenv.ID]*environment),
	}, nil
}

// Capabilities reports adapter-kind facts.
func (h *Host) Capabilities() execenv.Capabilities {
	return execenv.Capabilities{Isolated: false, Freeze: true, OrderedControl: true, JournalProjection: true, SemanticObservations: true}
}

// Ready reports configured images and remaining slots.
func (h *Host) Ready(ctx context.Context) (execenv.Report, error) {
	if err := ctx.Err(); err != nil {
		return execenv.Report{}, execenv.Error("ready", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	free := h.slots - len(h.grants)
	if free < 0 {
		free = 0
	}
	return execenv.Report{
		Usable:    !h.unusable,
		Images:    append([]execenv.Image(nil), h.images...),
		Networks:  []execenv.Network{execenv.NetworkNone, execenv.NetworkAllowlist},
		Slots:     free,
		Release:   execenv.Release,
		Isolation: execenv.IsolationNone,
	}, nil
}

// Ensure creates or returns the grant named by spec.
func (h *Host) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	if err := execenv.ValidateSpec(spec); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.unusable {
		return nil, execenv.Error("ensure", execenv.ErrUnavailable)
	}
	if env, ok := h.grants[spec.ID]; ok {
		if env.image != spec.Image || env.network != spec.Network {
			return nil, execenv.Error("ensure", execenv.ErrConflict)
		}
		return env, nil
	}
	if !h.hasImage(spec.Image) {
		return nil, execenv.Error("ensure", execenv.ErrUnknownImage)
	}
	if len(h.grants) >= h.slots {
		return nil, execenv.Error("ensure", execenv.ErrCapacity)
	}
	env := &environment{
		control:    control.New(spec.ID),
		projection: projection.New(),
		id:         spec.ID,
		image:      spec.Image,
		network:    spec.Network,
		host:       h,
		files:      make(tree.Snapshot),
		log:        watchlog.New(watchlog.DefaultCapacity),
	}
	h.grants[spec.ID] = env
	return env, nil
}

// Revoke destroys the grant if it exists.
func (h *Host) Revoke(ctx context.Context, id execenv.ID) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("revoke", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	env := h.grants[id]
	if env == nil {
		return nil
	}
	if err := env.revoke(); err != nil {
		return err
	}
	delete(h.grants, id)
	return nil
}

func (h *Host) revokeExact(ctx context.Context, env *environment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants[env.id] != env {
		return nil
	}
	if err := env.revoke(); err != nil {
		return err
	}
	delete(h.grants, env.id)
	return nil
}

func (h *Host) hasImage(want execenv.Image) bool {
	return slices.Contains(h.images, want)
}

func (e *environment) ID() execenv.ID { return e.id }

func (e *environment) Attach(ctx context.Context, win execenv.Window) (execenv.Terminal, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("attach", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revoked {
		return nil, execenv.Error("attach", execenv.ErrRevoked)
	}
	if e.frozen {
		return nil, execenv.Error("attach", execenv.ErrFrozen)
	}
	if e.term != nil && !e.term.finished() {
		return nil, execenv.Error("attach", execenv.ErrBusy)
	}
	term := newTerminal(e, win)
	e.term = term
	return term, nil
}

func (e *environment) Freeze(ctx context.Context) error {
	return e.control.Legacy(ctx, execenv.ControlFrozen, func() error { return e.freeze(ctx) })
}

func (e *environment) freeze(ctx context.Context) error {
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
		e.term.setFrozen(true)
	}
	e.failWatch(execenv.ErrFrozen)
	return nil
}

func (e *environment) Thaw(ctx context.Context) error {
	return e.control.Legacy(ctx, execenv.ControlRunning, func() error { return e.thaw(ctx) })
}

func (e *environment) thaw(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("thaw", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revoked {
		return execenv.Error("thaw", execenv.ErrRevoked)
	}
	e.frozen = false
	if e.term != nil {
		e.term.setFrozen(false)
	}
	return nil
}

func (e *environment) Revoke(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("revoke", err)
	}
	return e.host.revokeExact(ctx, e)
}

func (e *environment) revoke() error {
	return e.control.Legacy(context.Background(), execenv.ControlRevoked, e.stop)
}

func (e *environment) stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked = true
	e.frozen = false
	if e.term != nil {
		e.term.kill(execenv.ErrRevoked)
		e.term = nil
	}
	e.failWatch(execenv.ErrRevoked)
	return nil
}

func newTerminal(env *environment, win execenv.Window) *terminal {
	if win.Cols == 0 {
		win.Cols = 80
	}
	if win.Rows == 0 {
		win.Rows = 24
	}
	term := &terminal{env: env, win: win}
	term.cond = sync.NewCond(&term.mu)
	return term
}

func (t *terminal) setFrozen(frozen bool) {
	t.mu.Lock()
	t.frozen = frozen
	t.cond.Broadcast()
	t.mu.Unlock()
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
	for (t.frozen || len(t.buf) == 0) && !t.closed && t.dead == nil {
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
	if t.frozen {
		return 0, execenv.Error("terminal", execenv.ErrFrozen)
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
	if t.frozen {
		return execenv.Error("terminal", execenv.ErrFrozen)
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
