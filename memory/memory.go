// Package memory is an in-process execenv host for tests and local development.
package memory

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/control"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/observations"
	"github.com/sudosylabs/execenv/internal/projection"
	"github.com/sudosylabs/execenv/internal/terminal"
	"github.com/sudosylabs/execenv/internal/tree"
)

var (
	_ execenv.Host = (*Host)(nil)
	_ execenv.Env  = (*environment)(nil)
)

// Config constructs an in-memory host.
type Config struct {
	Images   []execenv.Image
	Slots    int
	Unusable bool
	// TerminalGrace is how long a detached shell keeps running. Zero uses
	// execenv.DefaultTerminalGrace.
	TerminalGrace time.Duration
}

// Host is an in-process execenv.Host. It is not isolated.
type Host struct {
	mu       sync.Mutex
	images   []execenv.Image
	slots    int
	unusable bool
	grace    time.Duration
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
	terminal        *terminal.Manager
	processes       *processLog
	// files is the current projection. Paths are already-validated.
	files tree.Snapshot
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
		grace:    cfg.TerminalGrace,
		grants:   make(map[execenv.ID]*environment),
	}, nil
}

// Capabilities reports adapter-kind facts.
func (h *Host) Capabilities() execenv.Capabilities {
	return execenv.Capabilities{Isolated: false}
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
		processes:  newProcessLog(),
	}
	// The in-memory shell is a stand-in, not a process; it starts with no
	// window or isolation beyond the session machinery every adapter shares.
	env.terminal = terminal.New(terminal.Config{
		Grace: h.grace,
		Start: func(_ context.Context, win execenv.Window) (terminal.Shell, error) {
			return newShell(win, env.processes), nil
		},
	})
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

func (e *environment) Attach(ctx context.Context, request execenv.AttachRequest) (execenv.Terminal, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("attach", err)
	}
	e.mu.Lock()
	err := e.guard("attach")
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return e.terminal.Attach(ctx, request)
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
	e.terminal.Freeze()
	return nil
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
	e.terminal.Thaw()
	return nil
}

func (e *environment) Revoke(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("revoke", err)
	}
	return e.host.revokeExact(ctx, e)
}

func (e *environment) revoke() error {
	return e.control.Revoke(context.Background(), e.stop)
}

func (e *environment) stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked = true
	e.frozen = false
	e.terminal.End(execenv.ErrRevoked)
	return nil
}
