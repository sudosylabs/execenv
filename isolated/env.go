package isolated

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/internal/control"
	"github.com/sudosylabs/execenv/internal/lifetime"
	"github.com/sudosylabs/execenv/internal/procs"
)

type environment struct {
	control *control.State
	// procs keeps the grant's processes: their history and input on the
	// host, the processes themselves in the guest.
	procs *procs.Manager
	mu    sync.Mutex
	id    execenv.ID
	spec  execenv.Spec
	image Image
	host  *Host
	timer *lifetime.Timer
	// public is what callers hold: the environment, watched for activity
	// when the grant has a timeout.
	public  execenv.Env
	inst    instance
	client  *agent.Client
	treeDir string
	frozen  bool
	paused  bool
	revoked bool
	// lost is set when the machine ended without a revoke. It implies
	// revoked: the environment is gone, but calls report ErrLost.
	lost bool
	// gone closes once the environment has been stopped, by revoke or loss.
	gone     chan struct{}
	goneOnce sync.Once
}

func (e *environment) ID() execenv.ID { return e.id }

func (e *environment) isLost() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lost
}

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
	// Verify refuses a helper that cannot serve the native journal, so Ensure
	// fails and the caller's error path revokes the machine.
	if err := client.Verify(ctx); err != nil {
		_ = client.Close()
		return err
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

// lossVerdict bounds how long a process whose link broke waits for the
// watcher's verdict.
const lossVerdict = 10 * time.Second

// endCause waits until the environment has been stopped and returns why:
// ErrLost or ErrRevoked. It returns fallback if no verdict comes in time.
func (e *environment) endCause(fallback error) error {
	timer := time.NewTimer(lossVerdict)
	defer timer.Stop()
	select {
	case <-e.gone:
	case <-timer.C:
		return fallback
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lost {
		return execenv.ErrLost
	}
	return execenv.ErrRevoked
}

// watch waits for the machine or its guest link to end. Either one ending
// before a revoke is a loss: the guest link cannot be re-established
// without losing the guest's processes and process log, so the host does
// not try.
func (e *environment) watch() {
	e.mu.Lock()
	inst, client := e.inst, e.client
	e.mu.Unlock()
	if inst == nil || client == nil {
		return
	}
	select {
	case <-e.gone:
		return
	case <-inst.Done():
	case <-client.Done():
	}
	e.host.lose(e)
}

// freeze stops process and journal I/O and pauses the microVM. The instance is
// kept so thaw does not boot a new machine.
func (e *environment) freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("freeze", err)
	}
	e.mu.Lock()
	if e.revoked {
		err := e.guard("freeze")
		e.mu.Unlock()
		return err
	}
	e.frozen = true
	client, inst, paused := e.client, e.inst, e.paused
	e.mu.Unlock()
	e.procs.Freeze()
	if paused {
		return nil
	}
	if client == nil || inst == nil {
		return execenv.Error("freeze", execenv.ErrUnavailable)
	}
	// Gate transport before stopping execution. Even an agent failure must not
	// prevent attempting containment at the host boundary.
	gateErr := client.Quiesce(ctx)
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

func (e *environment) thaw(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("thaw", err)
	}
	e.mu.Lock()
	if e.revoked {
		err := e.guard("thaw")
		e.mu.Unlock()
		return err
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
	if err := client.Reopen(ctx); err != nil {
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
	e.procs.Thaw()
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

// stop ends the environment for cause, ErrRevoked or ErrLost, and releases
// its machine. A failed release is retried by the next stop.
func (e *environment) stop(cause error) error {
	e.timer.Stop()
	e.mu.Lock()
	e.revoked = true
	if errors.Is(cause, execenv.ErrLost) {
		e.lost = true
	}
	e.frozen = false
	client := e.client
	e.client = nil
	inst := e.inst
	e.mu.Unlock()
	// Processes end with the cause before the verdict is out, so a reader
	// whose link broke sees why.
	e.procs.End(cause)
	e.goneOnce.Do(func() { close(e.gone) })
	if client != nil {
		client.Hangup(cause)
	}
	if client != nil {
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

// UpdateLimits replaces the grant's disk and network limits in its running
// machine.
func (e *environment) UpdateLimits(ctx context.Context, fence execenv.ProjectionFence, limits execenv.Limits) error {
	if err := execenv.ValidateLimits(limits); err != nil {
		return execenv.Error("update_limits", err)
	}
	return e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		inst := e.inst
		network := e.spec.Network
		e.mu.Unlock()
		if inst == nil {
			return execenv.Error("update_limits", execenv.ErrUnavailable)
		}
		if err := inst.UpdateLimits(ctx, limits, network == execenv.NetworkAllowlist); err != nil {
			return execenv.Error("update_limits", err)
		}
		e.mu.Lock()
		e.spec.Limits = limits
		e.mu.Unlock()
		return nil
	})
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
	if e.lost {
		return execenv.Error(op, execenv.ErrLost)
	}
	if e.revoked {
		return execenv.Error(op, execenv.ErrRevoked)
	}
	if e.frozen {
		return execenv.Error(op, execenv.ErrFrozen)
	}
	return nil
}
