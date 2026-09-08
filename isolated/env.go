package isolated

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/internal/control"
)

type environment struct {
	control *control.State
	mu      sync.Mutex
	id      execenv.ID
	image   Image
	network execenv.Network
	host    *Host
	inst    instance
	client  *agent.Client
	treeDir string
	frozen  bool
	paused  bool
	revoked bool
}

func (e *environment) ID() execenv.ID { return e.id }

func (e *environment) dial(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	inst := e.inst
	e.mu.Unlock()
	if inst == nil {
		return execenv.ErrRevoked
	}
	conn, err := inst.Connect(ctx)
	if err != nil {
		return err
	}
	client := agent.NewClient(conn)
	if err := client.Verify(ctx); err != nil {
		_ = client.Close()
		return err
	}
	if e.host.cfg.RequireJournal && !client.JournalEnabled() {
		_ = client.Close()
		return execenv.ErrUnavailable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revoked {
		_ = client.Close()
		return execenv.ErrRevoked
	}
	e.client = client
	return nil
}

func (e *environment) Attach(ctx context.Context, win execenv.Window) (execenv.Terminal, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("attach", err)
	}
	client, err := e.live("attach")
	if err != nil {
		return nil, err
	}
	return client.Attach(ctx, win)
}

// Freeze stops PTY and tree I/O and pauses the microVM. The instance is
// kept so Thaw does not boot a new machine.
func (e *environment) Freeze(ctx context.Context) error {
	return e.control.Legacy(ctx, execenv.ControlFrozen, func() error { return e.freeze(ctx) })
}

func (e *environment) freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("freeze", err)
	}
	e.mu.Lock()
	if e.revoked {
		e.mu.Unlock()
		return execenv.Error("freeze", execenv.ErrRevoked)
	}
	e.frozen = true
	client, inst, paused := e.client, e.inst, e.paused
	e.mu.Unlock()
	if paused {
		return nil
	}
	if client == nil || inst == nil {
		return execenv.Error("freeze", execenv.ErrUnavailable)
	}
	// Gate transport before stopping execution. Even an agent failure must not
	// prevent attempting containment at the host boundary.
	gateErr := client.Freeze(ctx)
	pauseErr := inst.Pause(ctx)
	if pauseErr != nil {
		return execenv.Error("freeze", errors.Join(gateErr, pauseErr))
	}
	e.mu.Lock()
	e.paused = true
	e.mu.Unlock()
	// A stopped instance is contained even if its agent link failed. Recovery
	// still requires a successful agent acknowledgement before opening I/O.
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
	if e.revoked {
		e.mu.Unlock()
		return execenv.Error("thaw", execenv.ErrRevoked)
	}
	client, inst, frozen := e.client, e.inst, e.frozen
	e.mu.Unlock()
	if !frozen {
		return nil
	}
	if client == nil || inst == nil {
		return execenv.Error("thaw", execenv.ErrUnavailable)
	}
	if err := inst.Resume(ctx); err != nil {
		return execenv.Error("thaw", err)
	}
	e.mu.Lock()
	e.paused = false
	e.mu.Unlock()
	if err := client.Thaw(ctx); err != nil {
		// A failed recovery never leaves a runnable guest behind an apparently
		// closed gate. Use a separate bounded context if the caller timed out.
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pauseErr := inst.Pause(recoveryCtx)
		e.mu.Lock()
		e.paused = pauseErr == nil
		e.mu.Unlock()
		return execenv.Error("thaw", errors.Join(err, pauseErr))
	}
	e.mu.Lock()
	e.frozen = false
	e.mu.Unlock()
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
	e.revoked = true
	e.frozen = false
	client := e.client
	e.client = nil
	inst := e.inst
	e.mu.Unlock()
	if client != nil {
		client.Hangup(execenv.ErrRevoked)
		_ = client.Close()
	}
	if inst != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := inst.Stop(stopCtx); err != nil {
			return err
		}
		e.mu.Lock()
		e.inst = nil
		e.mu.Unlock()
	}
	return nil
}

func (e *environment) live(op string) (*agent.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard(op); err != nil {
		return nil, err
	}
	if e.client == nil {
		return nil, execenv.Error(op, execenv.ErrUnavailable)
	}
	return e.client, nil
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
