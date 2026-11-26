package isolated

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/internal/control"
	"github.com/sudosylabs/execenv/internal/lifetime"
	"github.com/sudosylabs/execenv/internal/portconn"
	"github.com/sudosylabs/execenv/internal/procs"
)

type environment struct {
	ports portconn.Set

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
	// started is when the grant was first ensured; its wall timeout runs
	// from then, across restarts.
	started time.Time
	// adoptPending says the host takes over the guest's processes when the
	// grant thaws: it was adopted paused, or it was suspended.
	adoptPending bool
	// suspended says the grant's machine is saved to disk and stopped; a
	// thaw restores it.
	suspended bool
	// limitsDirty forces reconciliation after a monitor may have applied a
	// subset of an update or lost its response.
	limitsDirty bool
	// Deadlines retained while this host holds a saved guest.
	suspendedDeadlines map[string]procs.Deadlines
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

// watch waits for the machine to end. Its ending before a revoke is a loss.
func (e *environment) watch() {
	e.mu.Lock()
	inst := e.inst
	e.mu.Unlock()
	if inst == nil {
		return
	}
	done := inst.Done()
	select {
	case <-e.gone:
		return
	case <-done:
	}
	// A suspend stops the machine on purpose. A restored instance may
	// report a new completion channel.
	e.host.lose(e, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return !e.suspended && e.inst == inst && inst.Done() == done
	})
}

// watchLink waits for a guest link to end. A link that breaks while the
// host runs means the guest helper died, which is a loss; only a restarted
// host dials a guest again.
func (e *environment) watchLink(client *agent.Client) {
	if client == nil {
		return
	}
	select {
	case <-e.gone:
		return
	case <-client.Done():
	}
	e.host.lose(e, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.client == client
	})
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
	// A suspended machine is saved and stopped: frozen already.
	suspended := e.suspended
	e.mu.Unlock()
	e.manager().Freeze()
	e.ports.End(execenv.ErrFrozen)
	if paused || suspended {
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
	client, inst, frozen, pending, suspended := e.client, e.inst, e.frozen, e.adoptPending, e.suspended
	e.mu.Unlock()
	if !frozen {
		return nil
	}
	if suspended {
		if err := e.resume(ctx); err != nil {
			return execenv.Error("thaw", err)
		}
		return nil
	}
	if inst == nil || (client == nil && !pending) {
		return execenv.Error("thaw", execenv.ErrUnavailable)
	}
	if err := inst.Resume(ctx); err != nil {
		return e.recontainThaw(inst, err)
	}
	e.mu.Lock()
	e.paused = false
	e.mu.Unlock()
	// A grant adopted paused meets its guest now.
	if client == nil {
		if err := e.dial(ctx); err != nil {
			return e.recontainThaw(inst, err)
		}
		e.mu.Lock()
		client = e.client
		e.mu.Unlock()
		go e.watchLink(client)
	}
	if err := client.Reopen(ctx); err != nil {
		return e.recontainThaw(inst, err)
	}
	if pending {
		if err := e.adoptProcesses(ctx, client); err != nil {
			return e.recontainThaw(inst, err)
		}
		_ = os.RemoveAll(e.suspendDir())
		e.mu.Lock()
		e.adoptPending = false
		e.mu.Unlock()
	}
	e.manager().Thaw()
	e.mu.Lock()
	e.frozen = false
	e.mu.Unlock()
	e.timer.ArmIdle(e.spec.IdleTimeout)
	return nil
}

func (e *environment) recontainThaw(inst instance, err error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pauseErr := inst.Pause(ctx)
	e.mu.Lock()
	e.paused = pauseErr == nil
	e.mu.Unlock()
	if pauseErr != nil {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		return execenv.Error("thaw", errors.Join(err, pauseErr, inst.Stop(stopCtx)))
	}
	return execenv.Error("thaw", errors.Join(err, pauseErr))
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
	// Record the port cause before process teardown can cancel more
	// traffic on the shared guest link.
	e.ports.End(cause)
	// Process readers also see the grant's verdict.
	e.manager().End(cause)
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

// UpdateLimits replaces the grant's disk, network and port limits.
func (e *environment) UpdateLimits(ctx context.Context, fence execenv.ProjectionFence, limits execenv.Limits) error {
	if err := execenv.ValidateLimits(limits); err != nil {
		return execenv.Error("update_limits", err)
	}
	return e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		inst := e.inst
		network := e.spec.Network == execenv.NetworkAllowlist
		previous := e.spec.Limits
		dirty := e.limitsDirty
		e.mu.Unlock()
		if inst == nil {
			return execenv.Error("update_limits", execenv.ErrUnavailable)
		}
		diskChanged := limits.DiskBytes != previous.DiskBytes || limits.DiskOps != previous.DiskOps
		networkChanged := network && limits.NetworkBytes != previous.NetworkBytes
		updateDevices := dirty || diskChanged || networkChanged
		state := e.snapshotState(execenv.ControlReceipt{Fence: fence, State: execenv.ControlRunning, Confirmed: true})
		if updateDevices {
			state.LimitsDirty = true
			if err := e.writeState(state); err != nil {
				return err
			}
		}
		restore := func(err error) error {
			var restoreErr error
			if updateDevices {
				restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				restoreErr = inst.UpdateLimits(restoreCtx, previous, network)
				cancel()
			}
			state.Spec.Limits, state.LimitsDirty = previous, restoreErr != nil
			stateErr := e.writeState(state)
			e.mu.Lock()
			e.limitsDirty = restoreErr != nil || stateErr != nil
			e.mu.Unlock()
			return execenv.Error("update_limits", errors.Join(err, restoreErr, stateErr))
		}
		if updateDevices {
			if err := inst.UpdateLimits(ctx, limits, network); err != nil {
				return restore(err)
			}
		}
		state.Spec.Limits, state.LimitsDirty = limits, false
		if err := e.writeState(state); err != nil {
			return restore(err)
		}
		e.mu.Lock()
		e.spec.Limits = limits
		e.limitsDirty = false
		e.mu.Unlock()
		e.ports.SetLimit(limits.PortBytes)
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
