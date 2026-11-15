package execenv

import (
	"context"
	"time"
)

// GrantState is where a grant stands.
type GrantState string

const (
	// GrantCreating is a grant whose machine is being booted or restored.
	GrantCreating GrantState = "creating"
	// GrantRunning and GrantFrozen are its control state.
	GrantRunning GrantState = "running"
	GrantFrozen  GrantState = "frozen"
	// GrantSuspended is an idle grant saved to the host's disk.
	GrantSuspended GrantState = "suspended"
	// GrantRevoked has ended on request or by a timeout. It is the last
	// state a watch reports.
	GrantRevoked GrantState = "revoked"
	// GrantLost has ended without a revoke; it stays until revoked.
	GrantLost GrantState = "lost"
)

// GrantInfo describes one grant.
type GrantInfo struct {
	ID        ID
	Epoch     string
	Image     Image
	Network   Network
	Resources Resources
	Labels    map[string]string
	State     GrantState
	// FenceRevision is the revision of the control that put the grant in
	// its state, or 0 before the first.
	FenceRevision uint64
	// Started is when the grant was created.
	Started time.Time
}

// HostCapacity is a host's capacity and what is reserved of it.
type HostCapacity struct {
	CPUMillis   Capacity
	MemoryBytes Capacity
	DiskBytes   Capacity
}

// HostEvent is one change a host reports: a grant's new state, or its
// capacity.
type HostEvent struct {
	Grant    *GrantInfo
	Capacity *HostCapacity
}

// GrantWatch streams one grant's states. The first is its state now; each
// later one is a change, and a burst of changes may arrive as the last of
// them. After GrantRevoked, Next returns io.EOF.
type GrantWatch interface {
	Next(ctx context.Context) (GrantInfo, error)
	Close() error
}

// HostWatch streams a host's changes. It starts with every grant's state
// and the capacity now; then each change, a burst of changes to one grant
// or to the capacity arriving as the last of them.
type HostWatch interface {
	Next(ctx context.Context) (HostEvent, error)
	Close() error
}

// Grants lists and follows a host's grants, so a caller never polls.
type Grants interface {
	// ListGrants describes every grant the host holds, lost ones included.
	ListGrants(ctx context.Context) ([]GrantInfo, error)
	// WaitGrant returns once the grant has left GrantCreating. An unknown
	// grant is ErrNotFound.
	WaitGrant(ctx context.Context, id ID) (GrantInfo, error)
	// WatchGrant follows one grant. An unknown grant is ErrNotFound.
	WatchGrant(ctx context.Context, id ID) (GrantWatch, error)
	// WatchHost follows every grant and the capacity.
	WatchHost(ctx context.Context) (HostWatch, error)
}
