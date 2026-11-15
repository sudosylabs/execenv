package execenv

import (
	"context"
	"time"
)

// Stream is one output of a process: its terminal, or its stdout and
// stderr.
type Stream string

const (
	StreamPTY    Stream = "pty"
	StreamStdout Stream = "stdout"
	StreamStderr Stream = "stderr"
)

// Signal is what a caller may send a process.
type Signal string

const (
	SignalInterrupt Signal = "INT"
	SignalTerminate Signal = "TERM"
	SignalKill      Signal = "KILL"
	SignalHangup    Signal = "HUP"
	SignalQuit      Signal = "QUIT"
)

// Process limits.
const (
	// MaxProcesses is how many started processes a grant runs at once.
	MaxProcesses = 32
	// DefaultHistoryBytes and MaxHistoryBytes bound the output kept per
	// stream for resume.
	DefaultHistoryBytes = 1 << 20
	MaxHistoryBytes     = 4 << 20
	// MaxTagBytes bounds a process tag; tags use the id alphabet.
	MaxTagBytes = 64
)

// ProcessSpec starts one process in a grant. A shell is a process with a
// terminal; a command is one without.
type ProcessSpec struct {
	// Tag names the process. It is the caller's, unique among the grant's
	// running processes; every other call addresses the process by it.
	Tag string
	// Argv is the program and its arguments. Empty with a terminal is the
	// image's login shell.
	Argv []string
	// Env adds to the workload's environment; Dir is the working directory,
	// relative to the workspace ("" is the workspace).
	Env map[string]string
	Dir string
	// PTY, when set, gives the process a terminal of that size: one output
	// stream, and input to the terminal.
	PTY *Window
	// Stdin keeps a command's standard input open for WriteInput. Without
	// it a command's input is closed at once.
	Stdin bool
	// HistoryBytes is output kept per stream for resume: DefaultHistoryBytes
	// when zero, at most MaxHistoryBytes.
	HistoryBytes int64
	// DetachGrace, when set, ends the process once nothing has been
	// attached to it for that long. Zero never ends it for that reason.
	DetachGrace time.Duration
	// Timeout, when set, ends the process that long after it started: it is
	// sent TERM, then KILL five seconds later.
	Timeout time.Duration
}

// ExitStatus is how a process ended: an exit code, or the signal that
// killed it.
type ExitStatus struct {
	Code   int
	Signal int
}

// ProcessInfo describes one process.
type ProcessInfo struct {
	Tag     string
	PID     int
	PTY     bool
	Started time.Time
	// Running is false once the process has exited; Exit says how.
	Running bool
	Exit    ExitStatus
	// Output is the end offset of each stream: how many bytes it has
	// produced. Input is how many bytes of input the process has taken.
	Output map[Stream]uint64
	Input  uint64
}

// OutputKind is what one Output event carries.
type OutputKind uint8

const (
	// OutputData is bytes of one stream, at Offset.
	OutputData OutputKind = iota + 1
	// OutputTruncated says bytes from Offset to To were dropped from the
	// stream's history before this reader got them.
	OutputTruncated
	// OutputExit is the process's exit; it is always the last event.
	OutputExit
)

// Output is one event of an attachment.
type Output struct {
	Kind   OutputKind
	Stream Stream
	Offset uint64
	To     uint64
	Data   []byte
	Exit   ExitStatus
}

// Attachment reads one process's output from chosen offsets. Any number may
// be attached to a process at once; attaching keeps it from its
// DetachGrace.
type Attachment interface {
	// Next blocks for the next event. After OutputExit it returns io.EOF;
	// once the grant has ended it returns why (ErrRevoked, ErrLost).
	Next(ctx context.Context) (Output, error)
	// Close detaches.
	Close() error
}

// Screen is a terminal process's screen as of an output offset: bytes that
// redraw it on a blank terminal. Attaching from Offset then continues it.
type Screen struct {
	Data   []byte
	Offset uint64
}

// Processes runs processes in a grant. Starting and every input or signal
// carry the grant's fence; reading does not.
type Processes interface {
	Start(ctx context.Context, fence ProjectionFence, spec ProcessSpec) (ProcessInfo, error)
	Process(ctx context.Context, tag string) (ProcessInfo, error)
	ListProcesses(ctx context.Context) ([]ProcessInfo, error)
	// AttachProcess reads tag's output from the given offsets; a stream not
	// named starts at the oldest byte still held.
	AttachProcess(ctx context.Context, tag string, from map[Stream]uint64) (Attachment, error)
	// WriteInput appends data at offset. Bytes the process already took are
	// dropped, so a caller resending after a lost connection duplicates
	// nothing; an offset beyond what it took is ErrConflict. It returns the
	// new input offset.
	WriteInput(ctx context.Context, fence ProjectionFence, tag string, offset uint64, data []byte) (uint64, error)
	CloseInput(ctx context.Context, fence ProjectionFence, tag string) error
	Resize(ctx context.Context, fence ProjectionFence, tag string, window Window) error
	SignalProcess(ctx context.Context, fence ProjectionFence, tag string, signal Signal) error
}

// ValidateProcessSpec refuses a spec that can never start.
func ValidateProcessSpec(spec ProcessSpec) error {
	if len(spec.Tag) > MaxTagBytes || validateName(spec.Tag) != nil {
		return ErrInvalid
	}
	if len(spec.Argv) == 0 && spec.PTY == nil {
		return ErrInvalid
	}
	if len(spec.Argv) > MaxProcessArgs*4 {
		return ErrInvalid
	}
	if spec.Dir != "" && ValidatePath(spec.Dir) != nil {
		return ErrInvalid
	}
	for k := range spec.Env {
		if k == "" || len(k) > 256 {
			return ErrInvalid
		}
	}
	if spec.HistoryBytes < 0 || spec.HistoryBytes > MaxHistoryBytes || spec.DetachGrace < 0 || spec.Timeout < 0 {
		return ErrInvalid
	}
	return nil
}
