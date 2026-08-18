package isolated

import (
	"context"
	"sync"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
)

type environment struct {
	mu      sync.Mutex
	id      execenv.ID
	image   Image
	network execenv.Network
	host    *Host
	inst    instance
	client  *agent.Client
	treeDir string
	frozen  bool
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
	if err := ctx.Err(); err != nil {
		return execenv.Error("freeze", err)
	}
	e.mu.Lock()
	if e.revoked {
		e.mu.Unlock()
		return execenv.Error("freeze", execenv.ErrRevoked)
	}
	e.frozen = true
	client := e.client
	inst := e.inst
	e.mu.Unlock()
	if client != nil {
		_ = client.Freeze(ctx)
	}
	if inst != nil {
		_ = inst.Pause()
	}
	return nil
}

func (e *environment) Thaw(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("thaw", err)
	}
	e.mu.Lock()
	if e.revoked {
		e.mu.Unlock()
		return execenv.Error("thaw", execenv.ErrRevoked)
	}
	e.frozen = false
	client := e.client
	inst := e.inst
	e.mu.Unlock()
	// Resume the machine before the agent RPC. A paused guest cannot
	// answer Thaw; the stub agent is a host process and is unaffected.
	if inst != nil {
		_ = inst.Resume()
	}
	if client != nil {
		return client.Thaw(ctx)
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
	e.revoked = true
	e.frozen = false
	client := e.client
	e.client = nil
	inst := e.inst
	e.inst = nil
	e.mu.Unlock()
	if client != nil {
		client.Hangup(execenv.ErrRevoked)
		_ = client.Close()
	}
	if inst != nil {
		_ = inst.Stop()
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
