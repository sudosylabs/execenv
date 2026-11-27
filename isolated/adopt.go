package isolated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/internal/control"
	"github.com/sudosylabs/execenv/internal/lifetime"
	"github.com/sudosylabs/execenv/internal/procs"
)

// adoptTimeout bounds reconnecting to one adopted grant's guest.
const adoptTimeout = 30 * time.Second

// recover adopts the grants a previous host left running and releases
// everything else it left. It runs once, in New, under the work directory
// lock, before any caller can reach the host.
func (h *Host) recover() (result error) {
	defer func() {
		if result != nil {
			for _, env := range h.grants {
				if env.client != nil {
					_ = env.client.Close()
				}
			}
		}
	}()
	states, orphans, err := loadStates(h.cfg.WorkDir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	adopted := map[execenv.ID]instance{}
	var keep []machine
	for id, s := range states {
		if s.Machine == (machine{}) {
			continue
		}
		if inst, err := h.launch.Adopt(ctx, s.Machine); err == nil {
			if s.Lost || s.Suspended || !s.Receipt.Confirmed || s.Receipt.State == execenv.ControlRevoked ||
				!preservesNetworkPolicy(s.Spec.Network, s.Machine.Net, h.cfg.Allow) {
				stopCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				err := inst.Stop(stopCtx)
				stop()
				if err != nil {
					return err
				}
				continue
			}
			adopted[id] = inst
			keep = append(keep, s.Machine)
		}
	}
	if cleaner, ok := h.launch.(staleCleaner); ok {
		if err := cleaner.cleanStale(keep); err != nil {
			return err
		}
	}
	for _, dir := range orphans {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	for id, s := range states {
		if !s.Lost && s.Receipt.State == execenv.ControlRevoked {
			if err := os.RemoveAll(filepath.Dir(statePath(h.cfg.WorkDir, id))); err != nil {
				return err
			}
			continue
		}
		if s.Suspended && !s.Lost && s.Receipt.Confirmed {
			if err := h.adoptSuspended(s); err != nil {
				if stateErr := h.tombstone(s); stateErr != nil {
					return errors.Join(err, stateErr)
				}
			}
			continue
		}
		inst := adopted[id]
		if inst == nil {
			if err := h.tombstone(s); err != nil {
				return err
			}
			continue
		}
		actx, cancel := context.WithTimeout(ctx, adoptTimeout)
		err := h.adopt(actx, s, inst)
		cancel()
		if err != nil {
			stopCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			stopErr := inst.Stop(stopCtx)
			stop()
			if stopErr != nil {
				return errors.Join(err, stopErr)
			}
			if stateErr := h.tombstone(s); stateErr != nil {
				return errors.Join(err, stateErr)
			}
		}
	}
	for _, env := range h.grants {
		// Start callbacks after the complete recovery succeeds.
		if env.timer != nil {
			wall := env.spec.WallTimeout
			if wall > 0 {
				wall = max(wall-time.Since(env.started), time.Nanosecond)
			}
			idle := env.spec.IdleTimeout
			if env.suspended {
				idle = 0
			}
			env.timer.Start(wall, idle)
		}
		go env.watch()
		if env.client != nil {
			go env.watchLink(env.client)
		}
	}
	return nil
}

// tombstone keeps id lost across restarts: every call on its epoch fails
// with ErrLost, and Ensure of it too, until the caller revokes it.
func (h *Host) tombstone(s grantState) error {
	s.Lost = true
	s.Machine = machine{}
	if err := saveState(h.cfg.WorkDir, s); err != nil {
		return err
	}
	if err := clearLostGrant(h.cfg.WorkDir, s.Spec.ID); err != nil {
		return err
	}
	env := &environment{
		gone:    make(chan struct{}),
		retired: make(chan struct{}),
		control: control.Restore(s.Epoch, s.Receipt),
		id:      s.Spec.ID,
		spec:    s.Spec,
		host:    h,
		revoked: true,
		lost:    true,
		started: s.Started,
	}
	env.procs = procs.New(env.startProcess)
	env.procs.End(execenv.ErrLost)
	close(env.gone)
	_, _ = env.control.Lose(context.Background(), nil, func(execenv.ControlReceipt) error { return nil })
	h.lost[s.Spec.ID] = env
	h.lostCount++
	h.hub.Poke()
	return nil
}

func (h *Host) bootImage(s grantState) Image {
	if s.BootImage.ID == s.Spec.Image && ValidDigest(s.BootImage.Hash) {
		return s.BootImage
	}
	// Older records did not retain their artifact paths.
	if record, ok := h.images[s.Spec.Image]; ok {
		image, _ := record.verified()
		return image
	}
	return Image{}
}

// adopt takes over one grant: its capacity, its control state, its guest
// link and the processes its guest holds. A paused grant is adopted
// without its guest, which cannot answer until it is thawed.
func (h *Host) adopt(ctx context.Context, s grantState, inst instance) error {
	if err := h.pool.Reserve(s.Spec.ID, s.Spec.Resources); err != nil {
		return err
	}
	if s.LimitsDirty {
		if err := inst.UpdateLimits(ctx, s.Spec.Limits, s.Spec.Network == execenv.NetworkAllowlist); err != nil {
			h.pool.Release(s.Spec.ID)
			return err
		}
		s.LimitsDirty = false
		if err := saveState(h.cfg.WorkDir, s); err != nil {
			h.pool.Release(s.Spec.ID)
			return err
		}
	}
	image := h.bootImage(s)
	env := &environment{
		gone:    make(chan struct{}),
		retired: make(chan struct{}),
		control: control.Restore(s.Epoch, s.Receipt),
		id:      s.Spec.ID,
		spec:    s.Spec,
		image:   image,
		host:    h,
		inst:    inst,
		treeDir: filepath.Join(h.cfg.WorkDir, "grants", string(s.Spec.ID), workspaceName),
		started: s.Started,
	}
	env.ports.SetLimit(s.Spec.Limits.PortBytes)
	env.public = env
	env.procs = procs.New(env.startProcess)
	env.control.SetPersistence(env.persistReceipt)
	// A grant not running when its host went away is frozen in its guest
	// too: the guest's gate outlives the link.
	env.frozen = s.Paused || s.Receipt.State != execenv.ControlRunning
	env.paused = s.Paused
	if env.frozen {
		env.procs.Freeze()
	}
	if s.Paused {
		env.adoptPending = true
	} else {
		if err := env.dial(ctx); err != nil {
			h.pool.Release(s.Spec.ID)
			return err
		}
		if err := env.adoptProcesses(ctx, env.client); err != nil {
			_ = env.client.Close()
			h.pool.Release(s.Spec.ID)
			return err
		}
	}
	h.rememberAdopted(env)
	return nil
}

// adoptSuspended takes over a grant whose machine is saved: only its disk
// is reserved until a thaw restores it.
func (h *Host) adoptSuspended(s grantState) error {
	if _, err := os.Stat(filepath.Join(h.cfg.WorkDir, "grants", string(s.Spec.ID), suspendedName, jailState)); err != nil {
		return err
	}
	if err := h.pool.Reserve(s.Spec.ID, execenv.Resources{DiskBytes: s.Spec.Resources.DiskBytes}); err != nil {
		return err
	}
	image := h.bootImage(s)
	env := &environment{
		gone:         make(chan struct{}),
		retired:      make(chan struct{}),
		control:      control.Restore(s.Epoch, s.Receipt),
		id:           s.Spec.ID,
		spec:         s.Spec,
		image:        image,
		host:         h,
		treeDir:      filepath.Join(h.cfg.WorkDir, "grants", string(s.Spec.ID), workspaceName),
		started:      s.Started,
		frozen:       true,
		suspended:    true,
		adoptPending: true,
	}
	env.ports.SetLimit(s.Spec.Limits.PortBytes)
	env.public = env
	env.procs = procs.New(env.startProcess)
	env.procs.Freeze()
	env.control.SetPersistence(env.persistReceipt)
	h.rememberAdopted(env)
	return nil
}

func (h *Host) rememberAdopted(env *environment) {
	if env.spec.WallTimeout > 0 || env.spec.IdleTimeout > 0 {
		env.timer = lifetime.New(func(r lifetime.Reason) { h.expire(env, r) })
		env.public = lifetime.Watch(env, env.timer)
	}
	h.grants[env.id] = env
	env.control.OnChange(h.hub.Poke)
	h.hub.Poke()
}

// adoptProcesses takes over every process the guest holds, from the
// oldest output it still has.
func (e *environment) adoptProcesses(ctx context.Context, client *agent.Client) error {
	held, err := client.Processes(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	manager, deadlines := e.procs, e.suspendedDeadlines
	e.mu.Unlock()
	for _, gp := range held {
		if info, err := manager.Process(gp.Spec.Tag); err == nil && info.PID == gp.Info.PID && info.Started.Equal(gp.Info.Started) {
			continue
		}
		b, err := client.Adopt(gp)
		if err != nil {
			return err
		}
		adopted := procs.Adopted{Spec: gp.Spec, Info: gp.Info, From: gp.First}
		if saved, ok := deadlines[gp.Spec.Tag]; ok {
			adopted.Deadlines = &saved
		}
		if err := manager.Adopt(adopted, guestProcess{Backend: b, env: e, link: client.Done()}); err != nil {
			return err
		}
	}
	e.mu.Lock()
	e.suspendedDeadlines = nil
	e.mu.Unlock()
	return nil
}

// persistReceipt saves a grant before publication or under its control lock.
func (e *environment) persistReceipt(receipt execenv.ControlReceipt) error {
	return e.persistState(receipt, false)
}

func (e *environment) persistState(receipt execenv.ControlReceipt, lost bool) error {
	s := e.snapshotState(receipt)
	s.Lost = s.Lost || lost
	return e.writeState(s)
}

func (e *environment) snapshotState(receipt execenv.ControlReceipt) grantState {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := grantState{
		Spec: e.spec, BootImage: e.image, Epoch: e.control.Epoch(), Receipt: receipt,
		Started: e.started, Paused: e.paused, Suspended: e.suspended, Lost: e.lost,
		LimitsDirty: e.limitsDirty,
	}
	if e.inst != nil {
		s.Machine = e.inst.Machine()
	}
	return s
}

func (e *environment) writeState(s grantState) error {
	if err := saveState(e.host.cfg.WorkDir, s); err != nil {
		return execenv.Error("grant state", errors.Join(execenv.ErrUnavailable, err))
	}
	return nil
}
