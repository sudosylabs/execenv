package execenv

import "context"

// Image is a caller-facing name for an operator-preloaded guest runtime.
type Image string

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
	Usable bool
	Images []Image
	Slots  int
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
