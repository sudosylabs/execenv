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
// acknowledges it. ObserveProcesses reports, in order, every program the
// contained workload started and how each process ended, until the caller
// acknowledges it. Every projection, observation and process log call
// names the current fence, and a stale fence performs no I/O.
type Env interface {
	ID() ID
	// Epoch is the opaque identity of this occupancy. It is never reused
	// after the grant is revoked or reconstructed.
	Epoch() string
	Attach(ctx context.Context, request AttachRequest) (Terminal, error)
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
	Revoke(ctx context.Context) error
}

// TerminalID is the opaque identity of one terminal session: one shell and
// its output history. Every new shell gets a fresh value; it is never reused.
type TerminalID string

// Terminal session limits shared by every adapter.
const (
	// TerminalHistoryBytes is how much of a terminal session's most recent
	// output the host keeps for replay. Older output is overwritten.
	TerminalHistoryBytes = 1 << 20
	// TerminalTailBytes is how much recent output an attach without Resume
	// replays before live output.
	TerminalTailBytes = 16 << 10
	// DefaultTerminalGrace is how long a detached shell keeps running when
	// the host configures no grace.
	DefaultTerminalGrace = 60 * time.Second
)

// TerminalPosition names a byte in one terminal session's output. Offsets
// are absolute: offset 0 is the shell's first output byte.
type TerminalPosition struct {
	Terminal TerminalID
	// Offset is the next byte the client has not seen.
	Offset uint64
}

// AttachRequest asks for the grant's terminal session.
//
// A grant has at most one terminal session, and at most one client is
// attached to it. With no live session, Attach starts a shell and delivers
// from offset 0. With Resume naming the live session, Attach delivers from
// Resume.Offset, or from the start of the retained history when that output
// was overwritten; Start reports which. A Resume naming any other session is
// ErrNotFound, and an Offset beyond the output produced so far is
// ErrInvalid. Without Resume, Attach joins a live session, replays its last
// TerminalTailBytes and forces full-screen programs to redraw. Window
// applies on every attach.
type AttachRequest struct {
	Window Window
	// Resume continues a session from a known position. Nil attaches to
	// the live session, or starts one.
	Resume *TerminalPosition
	// Takeover replaces a currently attached client instead of returning
	// ErrBusy. The replaced Terminal's Read and Write return ErrSuperseded.
	Takeover bool
}

// Terminal is one client's attachment to a grant's terminal session.
//
// Read delivers the session's output from Start onward and returns io.EOF
// once the shell has exited and its output is drained. The host never
// blocks the shell on a slow reader: a reader that falls behind the
// retained history skips ahead to its oldest byte, which Offset makes
// visible as a jump. Input is accepted only from the attached client.
type Terminal interface {
	// ID is the session this attachment belongs to.
	ID() TerminalID
	// Start is the absolute offset of the first byte this attachment
	// delivers. Start greater than a requested Resume.Offset means the
	// output in between was lost.
	Start() uint64
	// Offset is the absolute offset of the next byte Read will return:
	// Start plus the bytes read, plus any output skipped by falling behind.
	Offset() uint64
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(ctx context.Context, win Window) error
	// Close detaches. The shell keeps running for the host's terminal
	// grace; a later Attach can resume it.
	Close() error
	// Hangup ends the shell now. The session is gone.
	Hangup(ctx context.Context) error
}
