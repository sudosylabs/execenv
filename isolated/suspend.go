package isolated

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/procs"
)

// suspendedName is where a suspended grant's saved machine lives, in its
// grant directory.
const suspendedName = "suspended"

func (e *environment) suspendDir() string {
	return filepath.Join(e.host.cfg.WorkDir, "grants", string(e.id), suspendedName)
}

// suspend freezes the grant, or keeps it frozen, saves its machine to disk
// and stops it, releasing its CPU and memory; its disk stays reserved. Its
// processes stay in the saved machine. A launcher that cannot save leaves
// the grant frozen, and a grant already suspended stays as it is. done is
// the control receipt the suspend ends with: the grant's state file
// records it, suspended, before the machine is stopped, so a host that
// dies in between finds the saved machine to resume.
func (e *environment) suspend(ctx context.Context, done execenv.ControlReceipt) error {
	e.mu.Lock()
	suspended := e.suspended
	e.mu.Unlock()
	if suspended {
		return e.finishSuspend(ctx, done)
	}
	// Freezing a frozen grant gates nothing new: its guest was quiesced
	// and its machine paused when it froze.
	if err := e.freeze(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	inst, client := e.inst, e.client
	s, ok := inst.(saver)
	var deadlines map[string]procs.Deadlines
	if ok {
		// Save can break the link. Preserve any pending deadline before
		// the old backend has a chance to report that it ended.
		deadlines = e.procs.SuspendDeadlines()
		// Saving breaks the guest link: it is detached first, so that is
		// not taken for a loss.
		e.client = nil
	}
	e.mu.Unlock()
	if !ok {
		return nil
	}
	if err := s.Save(ctx, e.suspendDir()); err != nil {
		_ = os.RemoveAll(e.suspendDir())
		e.mu.Lock()
		e.client = client
		e.mu.Unlock()
		if client != nil {
			select {
			case <-client.Done():
				go e.host.lose(e)
			default:
			}
		}
		return err
	}
	// The machine is saved. Its link and monitor go; its processes are in
	// what was saved, so the host's view of them is dropped without a
	// word to the guest, and taken over again on resume.
	e.mu.Lock()
	old := e.procs
	e.suspendedDeadlines = deadlines
	e.procs = procs.New(e.startProcess)
	e.procs.Freeze()
	e.client = nil
	e.suspended, e.adoptPending = true, true
	e.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	old.End(execenv.ErrFrozen)
	return e.finishSuspend(ctx, done)
}

func (e *environment) finishSuspend(ctx context.Context, done execenv.ControlReceipt) error {
	e.mu.Lock()
	inst := e.inst
	e.mu.Unlock()
	if inst == nil {
		return nil
	}
	if err := e.persistReceipt(done); err != nil {
		return err
	}
	return e.stopSuspendedMachine(ctx, inst)
}

func (e *environment) stopSuspendedMachine(ctx context.Context, inst instance) error {
	if err := inst.Stop(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.inst, e.paused = nil, false
	e.mu.Unlock()
	return e.host.pool.Replace(e.id, execenv.Resources{DiskBytes: e.spec.Resources.DiskBytes})
}

// resume restores a suspended grant's machine where it stopped. Until the
// grant's CPU and memory fit again it stays suspended, and the caller's
// thaw fails with ErrCapacity.
func (e *environment) resume(ctx context.Context) error {
	h := e.host
	e.mu.Lock()
	previous := e.inst
	e.mu.Unlock()
	if previous != nil {
		if err := e.stopSuspendedMachine(ctx, previous); err != nil {
			return err
		}
	}
	if err := h.pool.Replace(e.id, e.spec.Resources); err != nil {
		return err
	}
	fail := func(err error) error {
		return errors.Join(err, h.pool.Replace(e.id, execenv.Resources{DiskBytes: e.spec.Resources.DiskBytes}))
	}
	var att *netAttach
	if e.spec.Network == execenv.NetworkAllowlist {
		var err error
		att, err = h.attach.Setup(e.id, append([]string(nil), h.cfg.Allow...))
		if err != nil {
			return fail(err)
		}
	}
	inst, err := h.launch.Start(ctx, startRequest{
		ID:        e.id,
		Kernel:    e.image.Kernel,
		Rootfs:    e.image.Rootfs,
		TreeDir:   e.treeDir,
		Disk:      filepath.Join(filepath.Dir(e.treeDir), diskName),
		Resources: e.spec.Resources,
		Limits:    e.spec.Limits,
		Attach:    att,
		Epoch:     e.control.Epoch(),
		Restore:   &restoreFrom{Dir: e.suspendDir(), Own: true},
	})
	if err != nil {
		if inst != nil {
			e.mu.Lock()
			e.inst = inst
			e.mu.Unlock()
			if stopErr := e.stopSuspendedMachine(ctx, inst); stopErr != nil {
				return errors.Join(err, stopErr)
			}
			return err
		}
		if att != nil {
			att.close()
		}
		return fail(err)
	}
	e.mu.Lock()
	e.inst, e.suspended = inst, false
	e.mu.Unlock()
	go e.watch()
	if err := e.dial(ctx); err != nil {
		return e.recontainThaw(inst, err)
	}
	e.mu.Lock()
	client := e.client
	e.mu.Unlock()
	go e.watchLink(client)
	// The saved guest was quiesced before it was saved.
	if err := client.Reopen(ctx); err != nil {
		return e.recontainThaw(inst, err)
	}
	if err := e.adoptProcesses(ctx, client); err != nil {
		return e.recontainThaw(inst, err)
	}
	e.manager().Thaw()
	_ = os.RemoveAll(e.suspendDir())
	e.mu.Lock()
	e.adoptPending, e.frozen = false, false
	e.mu.Unlock()
	e.timer.ArmIdle(e.spec.IdleTimeout)
	return nil
}
