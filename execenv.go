package execenv

import (
	"context"
	"io"
)

// Image is a caller-facing name for an operator-preloaded guest runtime.
type Image string

// Paths the baked guest and the isolated adapter share. The image root is
// read-only. GuestWorkspace is the projected tree and the shell's working
// directory. GuestHome is the shell's $HOME: guest-local scratch for tool
// caches, virtual environments and dotfiles. It is never projected or
// observed, and nothing in it is harvested. Both live on the grant's
// writable workspace disk, not in guest memory.
const (
	GuestWorkspace = "/workspace"
	GuestHome      = "/run/execenv/home"
	GuestInit      = "/usr/local/libexec/execenv-guest"
	GuestBin       = "/usr/local/bin/execenv"
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
// Every adapter supports fenced control, journal projection and semantic
// observations; only isolation differs.
type Capabilities struct {
	Isolated bool
}

// Isolation names the class of boundary a host puts between a grant and
// the machine. It describes a property, not a product. Callers apply their
// own policy, for example refusing IsolationNone in production.
type Isolation string

const (
	// IsolationNone shares the host kernel and process space (tests and
	// local development).
	IsolationNone Isolation = "none"
	// IsolationVM runs each grant in a hardware-virtualized machine with
	// its own guest kernel.
	IsolationVM Isolation = "vm"
)

// Report is a point-in-time readiness snapshot.
type Report struct {
	Usable   bool
	Images   []Image
	Networks []Network
	Slots    int
	Release  string
	// Isolation is the boundary class this host provides.
	Isolation Isolation
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

// Env is one grant's occupancy for one epoch.
//
// Control is the only way to freeze, thaw or revoke under a fence; Revoke
// is the unfenced, terminal teardown. The projection is a caller-owned
// journal: upload immutable content, initialize once, then apply
// consecutive journal mutations. Observe reports what the isolated
// environment wrote, with its captured content, until the caller
// acknowledges it. Every projection and observation call names the current
// fence, and a stale fence performs no I/O.
type Env interface {
	ID() ID
	// Epoch is the opaque identity of this occupancy. It is never reused
	// after the grant is revoked or reconstructed.
	Epoch() string
	Attach(ctx context.Context, win Window) (Terminal, error)
	Control(ctx context.Context, request ControlRequest) (ControlReceipt, error)
	ControlStatus(ctx context.Context) (ControlReceipt, error)
	UploadProjectionContent(ctx context.Context, fence ProjectionFence, body io.Reader) (ContentReference, error)
	InitializeProjection(ctx context.Context, request ProjectionInitialization) (ProjectionReceipt, error)
	ApplyProjection(ctx context.Context, request ProjectionApply) (ProjectionReceipt, error)
	Observe(ctx context.Context, fence ProjectionFence, after uint64, limit int) (ObservationPage, error)
	OpenObservationContent(ctx context.Context, fence ProjectionFence, content ContentReference) (io.ReadCloser, error)
	ConfirmObservationProjection(ctx context.Context, binding ObservationProjection) error
	AcknowledgeObservations(ctx context.Context, fence ProjectionFence, through uint64) error
	Revoke(ctx context.Context) error
}

// Terminal is one PTY attached to an environment.
type Terminal interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(ctx context.Context, win Window) error
	Close() error
}
