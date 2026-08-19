package execenv

import (
	"context"
	"io"
)

// Image is a caller-facing name for an operator-preloaded guest runtime.
type Image string

// Paths the baked guest and the isolated adapter share. The image root
// is read-only; GuestHome is a tmpfs the agent owns.
const (
	GuestHome = "/workspace"
	GuestInit = "/usr/local/libexec/execenv-guest"
	GuestBin  = "/usr/local/bin/execenv"
)

// ID is a caller-chosen occupancy identity on one host.
type ID string

// Network selects whether a grant has no network or the host allowlist.
type Network uint8

const (
	// NetworkNone gives the grant no network.
	NetworkNone Network = iota
	// NetworkAllowlist gives the grant the host's configured allowlist.
	NetworkAllowlist
)

// Capabilities describes adapter-kind facts. They do not name a hypervisor.
type Capabilities struct {
	Isolated bool
	Freeze   bool
}

// Report is a point-in-time readiness snapshot.
type Report struct {
	Usable  bool
	Images  []Image
	Slots   int
	Release string
}

// Spec asks a host to occupy a grant.
type Spec struct {
	ID      ID
	Image   Image
	Network Network
}

// Window is a terminal size. Zero Cols or Rows means 80 by 24.
type Window struct {
	Cols uint16
	Rows uint16
}

// Host occupies grants on exactly one machine.
type Host interface {
	Capabilities() Capabilities
	Ready(ctx context.Context) (Report, error)
	Ensure(ctx context.Context, spec Spec) (Env, error)
	Revoke(ctx context.Context, id ID) error
}

// Env is one grant's occupancy.
type Env interface {
	ID() ID
	Attach(ctx context.Context, win Window) (Terminal, error)
	// ReplaceTree makes the projection match tree exactly. Nodes whose
	// Data is nil are kept only when the host already has that Path at
	// Version; otherwise the call fails and the projection is unchanged.
	ReplaceTree(ctx context.Context, tree Tree) error
	// Apply applies an atomic batch of incremental mutations.
	Apply(ctx context.Context, batch Batch) error
	// Watch streams guest-originated filesystem events. One observation
	// may be current. Overflow fails closed with ErrLagged.
	Watch(ctx context.Context) (Observation, error)
	// Open reads the current body of a projected file.
	Open(ctx context.Context, path string) (io.ReadCloser, error)
	Freeze(ctx context.Context) error
	Thaw(ctx context.Context) error
	Revoke(ctx context.Context) error
}

// Terminal is one PTY attached to an environment.
type Terminal interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(ctx context.Context, win Window) error
	Close() error
}
