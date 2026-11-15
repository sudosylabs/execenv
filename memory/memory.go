// Package memory is an in-process execenv host for tests and local development.
package memory

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/capacity"
	"github.com/sudosylabs/execenv/internal/control"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/lifetime"
	"github.com/sudosylabs/execenv/internal/observations"
	"github.com/sudosylabs/execenv/internal/projection"
	"github.com/sudosylabs/execenv/internal/terminal"
	"github.com/sudosylabs/execenv/internal/tree"
)

var (
	_ execenv.Host = (*Host)(nil)
	_ execenv.Env  = (*environment)(nil)
)

// DefaultCapacity is a Config's capacity when it sets none: room for four
// grants of execenv.DefaultGrant.
var DefaultCapacity = execenv.DefaultGrant.Times(4)

// Config constructs an in-memory host.
type Config struct {
	Images []execenv.Image
	// Capacity is the host's totals, Defaults what a Spec's zero Resources
	// fields mean, and GrantMax the most one grant may ask for. Zero values
	// use DefaultCapacity, execenv.DefaultGrant and Capacity.
	Capacity execenv.Resources
	Defaults execenv.Resources
	GrantMax execenv.Resources
	Unusable bool
	// TerminalGrace is how long a detached shell keeps running. Zero uses
	// execenv.DefaultTerminalGrace.
	TerminalGrace time.Duration
}

// Host is an in-process execenv.Host. It is not isolated.
type Host struct {
	mu       sync.Mutex
	images   []execenv.Image
	pool     *capacity.Pool
	unusable bool
	grace    time.Duration
	grants   map[execenv.ID]*environment
	// lost holds grants whose environment ended without a revoke until the
	// caller revokes them.
	lost      map[execenv.ID]*environment
	lostCount int
	draining  bool
}

type environment struct {
	control         *control.State
	projection      *projection.State
	semantic        *observations.Log
	guestTree       *guesttree.Snapshot
	projectionFence execenv.ProjectionFence
	mu              sync.Mutex
	id              execenv.ID
	spec            execenv.Spec
	host            *Host
	frozen          bool
	revoked         bool
	lost            bool
	terminal        *terminal.Manager
	processes       *processLog
	limits          execenv.Limits
	timer           *lifetime.Timer
	// public is what callers hold: the environment, watched for activity.
	public execenv.Env
	// files is the current projection. Paths are already-validated.
	files tree.Snapshot
}

// New returns an inert in-memory host.
func New(cfg Config) (*Host, error) {
	total := cfg.Capacity
	if total == (execenv.Resources{}) {
		total = DefaultCapacity
	}
	pool, err := capacity.New(total, cfg.Defaults, cfg.GrantMax)
	if err != nil {
		return nil, execenv.Error("memory", err)
	}
	return &Host{
		images:   append([]execenv.Image(nil), cfg.Images...),
		pool:     pool,
		unusable: cfg.Unusable,
		grace:    cfg.TerminalGrace,
		grants:   make(map[execenv.ID]*environment),
		lost:     make(map[execenv.ID]*environment),
	}, nil
}

// Capabilities reports adapter-kind facts.
func (h *Host) Capabilities() execenv.Capabilities {
	return execenv.Capabilities{Isolated: false}
}

// SetDraining starts or stops draining. A draining host keeps serving the
// grants it holds, including Ensure of one of them, refuses new grants with
// execenv.ErrDraining, and reports Usable false with
// execenv.ReasonDraining. It is operator control, not part of the
// execenv.Host contract.
func (h *Host) SetDraining(on bool) {
	h.mu.Lock()
	h.draining = on
	h.mu.Unlock()
}

// Lose ends a grant's environment as if its machine had died, without a
// revoke: its resources are freed and every call on the occupancy fails
// with execenv.ErrLost until the grant is revoked. It reports whether a live
// grant was lost. It is a test hook; nothing else loses an in-memory grant.
func (h *Host) Lose(id execenv.ID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	env := h.grants[id]
	if env == nil {
		return false
	}
	lost, _ := env.control.Lose(context.Background(), func() error { return env.stop(execenv.ErrLost) })
	if !lost {
		return false
	}
	h.lostCount++
	h.lost[id] = env
	delete(h.grants, id)
	h.pool.Release(id)
	return true
}

// Ready reports configured images and free capacity.
func (h *Host) Ready(ctx context.Context) (execenv.Report, error) {
	if err := ctx.Err(); err != nil {
		return execenv.Report{}, execenv.Error("ready", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	usable, reason := !h.unusable, ""
	switch {
	case !usable:
		reason = execenv.ReasonUnavailable
	case h.draining:
		usable, reason = false, execenv.ReasonDraining
	}
	report := execenv.Report{
		Usable:    usable,
		Reason:    reason,
		Grants:    len(h.grants),
		Lost:      h.lostCount,
		Images:    append([]execenv.Image(nil), h.images...),
		Networks:  []execenv.Network{execenv.NetworkNone, execenv.NetworkAllowlist},
		Release:   execenv.Release,
		Isolation: execenv.IsolationNone,
	}
	h.pool.Fill(&report)
	return report, nil
}

// Ensure creates or returns the grant named by spec.
func (h *Host) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	if err := execenv.ValidateSpec(spec); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	spec.Resources = h.pool.Resolve(spec.Resources)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.unusable {
		return nil, execenv.Error("ensure", execenv.ErrUnavailable)
	}
	if _, lost := h.lost[spec.ID]; lost {
		return nil, execenv.Error("ensure", execenv.ErrLost)
	}
	if env, ok := h.grants[spec.ID]; ok {
		if !execenv.SameGrant(env.spec, spec) {
			return nil, execenv.Error("ensure", execenv.ErrConflict)
		}
		return env.public, nil
	}
	if h.draining {
		return nil, execenv.Error("ensure", execenv.ErrDraining)
	}
	if !h.hasImage(spec.Image) {
		return nil, execenv.Error("ensure", execenv.ErrUnknownImage)
	}
	if err := h.pool.Reserve(spec.ID, spec.Resources); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	env := &environment{
		control:    control.New(spec.ID),
		projection: projection.New(),
		id:         spec.ID,
		spec:       spec,
		host:       h,
		files:      make(tree.Snapshot),
		processes:  newProcessLog(),
		limits:     spec.Limits,
	}
	// The in-memory shell is a stand-in, not a process; it starts with no
	// window or isolation beyond the session machinery every adapter shares.
	env.terminal = terminal.New(terminal.Config{
		Grace: h.grace,
		Start: func(_ context.Context, win execenv.Window) (terminal.Shell, error) {
			return newShell(win, env.processes), nil
		},
	})
	env.public = env
	if spec.WallTimeout > 0 || spec.IdleTimeout > 0 {
		env.timer = lifetime.Start(spec.WallTimeout, spec.IdleTimeout, func(r lifetime.Reason) { h.expire(env, r) })
		env.public = lifetime.Watch(env, env.timer)
	}
	h.grants[spec.ID] = env
	return env.public, nil
}

// expire applies a grant's wall or idle timeout.
func (h *Host) expire(env *environment, r lifetime.Reason) {
	if r == lifetime.Idle && env.spec.IdleAction == execenv.IdleFreeze {
		_, _ = env.control.Idle(context.Background(), func() error { return env.freeze(context.Background()) })
		return
	}
	_ = h.revokeExact(context.Background(), env)
}

// Revoke destroys the grant if it exists.
func (h *Host) Revoke(ctx context.Context, id execenv.ID) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("revoke", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.lost, id)
	env := h.grants[id]
	if env == nil {
		return nil
	}
	if err := env.revoke(); err != nil {
		return err
	}
	delete(h.grants, id)
	h.pool.Release(id)
	return nil
}

func (h *Host) revokeExact(ctx context.Context, env *environment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lost[env.id] == env {
		delete(h.lost, env.id)
	}
	if h.grants[env.id] != env {
		return nil
	}
	if err := env.revoke(); err != nil {
		return err
	}
	delete(h.grants, env.id)
	h.pool.Release(env.id)
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

// UpdateLimits records the limits. The in-memory host has no devices to
// throttle.
func (e *environment) UpdateLimits(ctx context.Context, fence execenv.ProjectionFence, limits execenv.Limits) error {
	if err := execenv.ValidateLimits(limits); err != nil {
		return execenv.Error("update_limits", err)
	}
	return e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		e.limits = limits
		e.mu.Unlock()
		return nil
	})
}

func (e *environment) freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("freeze", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revoked {
		return e.guard("freeze")
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
		return e.guard("thaw")
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
	return e.control.Revoke(context.Background(), func() error { return e.stop(execenv.ErrRevoked) })
}

// stop ends the environment for cause, ErrRevoked or ErrLost.
func (e *environment) stop(cause error) error {
	e.timer.Stop()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked = true
	e.lost = errors.Is(cause, execenv.ErrLost) || e.lost
	e.frozen = false
	e.terminal.End(cause)
	return nil
}
