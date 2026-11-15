package execenv

import (
	"context"
	"io"
	"time"
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
	// Usable is whether the host occupies new grants now. A host that is
	// not usable may still serve the grants it holds; Reason says why.
	Usable bool
	// Reason is a short operator-facing explanation when Usable is false,
	// such as ReasonDraining. It is empty when Usable is true.
	Reason   string
	Images   []Image
	Networks []Network
	// CPUMillis, MemoryBytes and DiskBytes are the host's capacity. Every
	// grant reserves its whole Resources from the moment it starts until it
	// is revoked or lost; a grant being started counts at once. CPU totals
	// already include the operator's overcommit; memory is never
	// overcommitted.
	CPUMillis   Capacity
	MemoryBytes Capacity
	DiskBytes   Capacity
	// Defaults is what a Spec's zero Resources fields mean on this host,
	// and GrantMax the most one grant may ask for.
	Defaults Resources
	GrantMax Resources
	// Grants is how many grants the host holds now, including those still
	// starting. Lost grants are not counted.
	Grants int
	// Lost is how many grants this host process has lost since it started:
	// environments that ended without a revoke.
	Lost    int
	Release string
	// Isolation is the boundary class this host provides.
	Isolation Isolation
}

// Report reasons. Adapters may report other text; these are stable.
const (
	// ReasonDraining means the operator is draining the host: it serves
	// the grants it holds and refuses new ones with ErrDraining.
	ReasonDraining = "draining"
	// ReasonUnavailable means the host cannot occupy grants at all, for
	// example because its isolation is missing.
	ReasonUnavailable = "unavailable"
)

// Spec asks a host to occupy a grant. Ensure is idempotent on ID: the same
// spec returns the same grant, and a different one, other than in Limits,
// is ErrConflict.
type Spec struct {
	ID      ID
	Image   Image
	Network Network
	// Resources is what the grant is given. Zero fields use the host's
	// Defaults. A grant that does not fit is a *CapacityError at once; the
	// host never queues it.
	Resources Resources
	// WallTimeout, when set, revokes the grant that long after it started.
	WallTimeout time.Duration
	// IdleTimeout and IdleAction, set together, freeze or revoke the grant
	// once it has been idle that long. A grant is busy while a terminal is
	// attached to it and for a moment after any call on it.
	IdleTimeout time.Duration
	IdleAction  IdleAction
	// Labels are the caller's, for its own bookkeeping. The host never
	// interprets them.
	Labels map[string]string
	// Limits throttle the grant's devices from its start.
	Limits Limits
	// Seed, when set, is the digest of a seed this host holds: the grant's
	// workspace starts as a copy of it. An unknown seed is ErrNotFound.
	Seed string
}

// Window is a terminal size. Zero Cols or Rows means 80 by 24.
type Window struct {
	Cols uint16
	Rows uint16
}

// Host occupies grants on exactly one machine.
//
// A grant's environment can end without a revoke: its machine stops or its
// guest crashes. The host notices on its own, releases the grant's slot and
// resources, and fails every later call on that occupancy, and Ensure of the
// same id, with ErrLost until the caller calls Revoke for it.
type Host interface {
	Capabilities() Capabilities
	Ready(ctx context.Context) (Report, error)
	Ensure(ctx context.Context, spec Spec) (Env, error)
	Revoke(ctx context.Context, id ID) error
	Seeder
}

// Env is one grant's occupancy for one epoch.
//
// Once the environment is lost (see ErrLost), every method except Revoke
// fails with ErrLost, and so do the Read, Write and Resize of its terminal
// attachments. Revoke acknowledges the loss and returns nil.
//
// Control is the only way to freeze, thaw or revoke under a fence; Revoke
// is the unfenced, terminal teardown. The projection is a caller-owned
// journal: upload immutable content, initialize once, then apply
// consecutive journal mutations. Observe reports what the isolated
// environment wrote, with its captured content, until the caller
// acknowledges it. ObserveProcesses reports, in order, every program the
// contained workload started and how each process ended, until the caller
// acknowledges it. Every projection, observation and process log call
// names the current fence, and a stale fence performs no I/O.
type Env interface {
	ID() ID
	// Epoch is the opaque identity of this occupancy. It is never reused
	// after the grant is revoked or reconstructed.
	Epoch() string
	Control(ctx context.Context, request ControlRequest) (ControlReceipt, error)
	ControlStatus(ctx context.Context) (ControlReceipt, error)
	UploadProjectionContent(ctx context.Context, fence ProjectionFence, body io.Reader) (ContentReference, error)
	InitializeProjection(ctx context.Context, request ProjectionInitialization) (ProjectionReceipt, error)
	ApplyProjection(ctx context.Context, request ProjectionApply) (ProjectionReceipt, error)
	Observe(ctx context.Context, fence ProjectionFence, after uint64, limit int) (ObservationPage, error)
	OpenObservationContent(ctx context.Context, fence ProjectionFence, content ContentReference) (io.ReadCloser, error)
	ConfirmObservationProjection(ctx context.Context, binding ObservationProjection) error
	AcknowledgeObservations(ctx context.Context, fence ProjectionFence, through uint64) error
	// ObserveProcesses returns up to limit process log events after the
	// given sequence. A position before the acknowledged prefix is
	// ErrLagged; one beyond the events produced so far, or a limit outside
	// 1..MaxProcessPage, is ErrInvalid.
	ObserveProcesses(ctx context.Context, fence ProjectionFence, after uint64, limit int) (ProcessPage, error)
	// AcknowledgeProcesses releases the retained events through a
	// sequence. Acknowledging past the events produced is ErrInvalid; an
	// older acknowledgement is a no-op.
	AcknowledgeProcesses(ctx context.Context, fence ProjectionFence, through uint64) error
	// UpdateLimits replaces the grant's device limits while it runs.
	UpdateLimits(ctx context.Context, fence ProjectionFence, limits Limits) error
	Revoke(ctx context.Context) error
	Files
	Processes
}
