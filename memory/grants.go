package memory

import (
	"context"

	"github.com/sudosylabs/execenv"
)

// grantSnapshot is the hub's view of the held and lost grants, and the
// capacity.
func (h *Host) grantSnapshot() ([]execenv.GrantInfo, execenv.HostCapacity) {
	var r execenv.Report
	h.pool.Fill(&r)
	capacity := execenv.HostCapacity{CPUMillis: r.CPUMillis, MemoryBytes: r.MemoryBytes, DiskBytes: r.DiskBytes}
	h.mu.Lock()
	held := make([]*environment, 0, len(h.grants)+len(h.lost))
	for _, env := range h.grants {
		held = append(held, env)
	}
	for id, env := range h.lost {
		if h.grants[id] == nil {
			held = append(held, env)
		}
	}
	h.mu.Unlock()
	out := make([]execenv.GrantInfo, 0, len(held))
	for _, env := range held {
		if info, ok := env.info(); ok {
			out = append(out, info)
		}
	}
	return out, capacity
}

// info describes the grant, or reports it ended.
func (e *environment) info() (execenv.GrantInfo, bool) {
	receipt, lost := e.control.Peek()
	e.mu.Lock()
	revoked := e.revoked
	e.mu.Unlock()
	info := execenv.GrantInfo{
		ID: e.id, Epoch: e.control.Epoch(), Image: e.spec.Image, Network: e.spec.Network,
		Resources: e.spec.Resources, Labels: e.spec.Labels, FenceRevision: receipt.Fence.FenceRevision, Started: e.started,
	}
	switch {
	case lost:
		info.State = execenv.GrantLost
	case revoked || receipt.State == execenv.ControlRevoked:
		return execenv.GrantInfo{}, false
	case receipt.State == execenv.ControlFrozen:
		info.State = execenv.GrantFrozen
	default:
		info.State = execenv.GrantRunning
	}
	return info, true
}

func (h *Host) ListGrants(ctx context.Context) ([]execenv.GrantInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("list_grants", err)
	}
	return h.hub.List(), nil
}

func (h *Host) WaitGrant(ctx context.Context, id execenv.ID) (execenv.GrantInfo, error) {
	g, err := h.hub.Wait(ctx, id)
	if err != nil {
		return execenv.GrantInfo{}, execenv.Error("wait_grant", err)
	}
	return g, nil
}

func (h *Host) WatchGrant(_ context.Context, id execenv.ID) (execenv.GrantWatch, error) {
	w, err := h.hub.WatchGrant(id)
	if err != nil {
		return nil, execenv.Error("watch_grant", err)
	}
	return w, nil
}

func (h *Host) WatchHost(context.Context) (execenv.HostWatch, error) {
	return h.hub.WatchHost(), nil
}
