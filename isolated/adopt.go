package isolated

import (
	"context"
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
		if s.Lost || s.Suspended {
			continue
		}
		if inst, err := h.launch.Adopt(ctx, s.Machine); err == nil {
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
		if s.Suspended && !s.Lost {
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
			_ = inst.Stop(stopCtx)
			stop()
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
	env.public = env
	env.procs = procs.New(env.startProcess)
	// A grant not running when its host went away is frozen in its guest
	// too: the guest's gate outlives the link.
	env.frozen = s.Paused || s.Receipt.State != execenv.ControlRunning
	env.paused = s.Paused
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
	if env.frozen {
		env.procs.Freeze()
	}
	if s.Spec.WallTimeout > 0 || s.Spec.IdleTimeout > 0 {
		wall := s.Spec.WallTimeout
		if wall > 0 {
			wall = max(wall-time.Since(s.Started), time.Nanosecond)
		}
		env.timer = lifetime.Start(wall, s.Spec.IdleTimeout, func(r lifetime.Reason) { h.expire(env, r) })
		env.public = lifetime.Watch(env, env.timer)
	}
	h.grants[s.Spec.ID] = env
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
	env.public = env
	env.procs = procs.New(env.startProcess)
	env.procs.Freeze()
	if s.Spec.WallTimeout > 0 {
		wall := max(s.Spec.WallTimeout-time.Since(s.Started), time.Nanosecond)
		env.timer = lifetime.Start(wall, 0, func(r lifetime.Reason) { h.expire(env, r) })
		env.public = lifetime.Watch(env, env.timer)
	}
	h.grants[s.Spec.ID] = env
	return nil
}

// adoptProcesses takes over every process the guest holds, from the
// oldest output it still has.
func (e *environment) adoptProcesses(ctx context.Context, client *agent.Client) error {
	held, err := client.Processes(ctx)
	if err != nil {
		return err
	}
	for _, gp := range held {
		b, err := client.Adopt(gp)
		if err != nil {
			return err
		}
		adopted := procs.Adopted{Spec: gp.Spec, Started: gp.Info.Started, Input: gp.Info.Input, From: gp.First, Until: gp.Info.Output}
		if err := e.manager().Adopt(adopted, guestProcess{Backend: b, env: e, link: client.Done()}); err != nil {
			return err
		}
	}
	return nil
}

// persist records the grant's state for a restarted host. A failure is
// not the caller's: the grant runs on, and only a restart would lose it.
func (e *environment) persist() {
	receipt, _ := e.control.Status(context.Background())
	e.mu.Lock()
	if e.revoked || (e.inst == nil && !e.suspended) {
		e.mu.Unlock()
		return
	}
	s := grantState{Spec: e.spec, Epoch: e.control.Epoch(), Receipt: receipt, Started: e.started, Paused: e.paused, Suspended: e.suspended}
	if e.inst != nil {
		s.Machine = e.inst.Machine()
	}
	e.mu.Unlock()
	_ = saveState(e.host.cfg.WorkDir, s)
}
