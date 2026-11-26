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
func (h *Host) recover() error {
	states, orphans, err := loadStates(h.cfg.WorkDir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	adopted := map[execenv.ID]instance{}
	var keep []machine
	for id, s := range states {
		if s.Lost || s.Suspended && s.Machine == (machine{}) {
			continue
		}
		if inst, err := h.launch.Adopt(ctx, s.Machine); err == nil {
			if s.Suspended || !s.Receipt.Confirmed || s.Receipt.State == execenv.ControlRevoked ||
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
				h.tombstone(s)
			}
			continue
		}
		inst := adopted[id]
		if inst == nil {
			h.tombstone(s)
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
			h.tombstone(s)
		}
	}
	return nil
}

// tombstone keeps id lost across restarts: every call on its epoch fails
// with ErrLost, and Ensure of it too, until the caller revokes it.
func (h *Host) tombstone(s grantState) {
	env := &environment{
		gone:    make(chan struct{}),
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
	_, _ = env.control.Lose(context.Background(), func() error { return nil })
	h.lost[s.Spec.ID] = env
	h.lostCount++
	h.hub.Poke()
	dir := filepath.Join(h.cfg.WorkDir, "grants", string(s.Spec.ID))
	_ = os.RemoveAll(dir)
	if os.MkdirAll(dir, 0o700) == nil {
		s.Lost = true
		_ = saveState(h.cfg.WorkDir, s)
	}
}

// adopt takes over one grant: its capacity, its control state, its guest
// link and the processes its guest holds. A paused grant is adopted
// without its guest, which cannot answer until it is thawed.
func (h *Host) adopt(ctx context.Context, s grantState, inst instance) error {
	if err := h.pool.Reserve(s.Spec.ID, s.Spec.Resources); err != nil {
		return err
	}
	var image Image
	if record, ok := h.images[s.Spec.Image]; ok {
		image, _ = record.verified()
	}
	env := &environment{
		gone:    make(chan struct{}),
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
	if s.Spec.WallTimeout > 0 || s.Spec.IdleTimeout > 0 {
		wall := s.Spec.WallTimeout
		if wall > 0 {
			wall = max(wall-time.Since(s.Started), time.Nanosecond)
		}
		env.timer = lifetime.New(func(r lifetime.Reason) { h.expire(env, r) })
		env.public = lifetime.Watch(env, env.timer)
		env.timer.Start(wall, s.Spec.IdleTimeout)
	}
	h.grants[s.Spec.ID] = env
	env.control.OnChange(h.hub.Poke)
	h.hub.Poke()
	go env.watch()
	if env.client != nil {
		go env.watchLink(env.client)
	}
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
	var image Image
	if record, ok := h.images[s.Spec.Image]; ok {
		image, _ = record.verified()
	}
	env := &environment{
		gone:         make(chan struct{}),
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
	if s.Spec.WallTimeout > 0 || s.Spec.IdleTimeout > 0 {
		var wall time.Duration
		if s.Spec.WallTimeout > 0 {
			wall = max(s.Spec.WallTimeout-time.Since(s.Started), time.Nanosecond)
		}
		env.timer = lifetime.New(func(r lifetime.Reason) { h.expire(env, r) })
		env.public = lifetime.Watch(env, env.timer)
		env.timer.Start(wall, 0)
	}
	h.grants[s.Spec.ID] = env
	env.control.OnChange(h.hub.Poke)
	h.hub.Poke()
	return nil
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
	e.mu.Lock()
	s := grantState{Spec: e.spec, Epoch: e.control.Epoch(), Receipt: receipt, Started: e.started, Paused: e.paused, Suspended: e.suspended}
	s.Lost = e.lost
	if e.inst != nil {
		s.Machine = e.inst.Machine()
	}
	e.mu.Unlock()
	if err := saveState(e.host.cfg.WorkDir, s); err != nil {
		return execenv.Error("grant state", errors.Join(execenv.ErrUnavailable, err))
	}
	return nil
}
