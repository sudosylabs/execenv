package execenv

import (
	"path"
	"time"
	"unicode/utf8"
)

// Process log limits shared by every adapter.
const (
	// MaxProcessArgs and MaxProcessArgvBytes bound one started command
	// line. A longer one keeps its leading arguments, cuts the last kept
	// argument at a character boundary, and sets Truncated.
	MaxProcessArgs      = 64
	MaxProcessArgvBytes = 1 << 10
	// MaxProcessDirBytes bounds a reported working directory. A longer one
	// is reported as "".
	MaxProcessDirBytes = 4096
	// MaxProcessEvents and MaxProcessBytes bound the unacknowledged
	// process log the host retains for one epoch. Bytes count each event's
	// JSON encoding.
	MaxProcessEvents = 4096
	MaxProcessBytes  = 2 << 20
	// MaxProcessPage bounds the events in one ObserveProcesses page. A page
	// also stops before its JSON encoding would exceed
	// MaxProjectionMetadataBytes.
	MaxProcessPage = 256
	// maxProcessPID is the kernel's largest process id.
	maxProcessPID = 1 << 22
)

// ProcessKind says what a ProcessEvent records.
type ProcessKind string

const (
	// ProcessStarted records a workload process starting a program.
	ProcessStarted ProcessKind = "started"
	// ProcessExited records a started process ending.
	ProcessExited ProcessKind = "exited"
	// ProcessDropped stands in for events that were lost before it.
	ProcessDropped ProcessKind = "dropped"
)

// ProcessEvent is one entry in an environment's process log: a program the
// contained workload started, how a started process ended, or a count of
// lost events. The guest helper records it outside the workload, so the
// workload cannot see, alter or suppress an entry already taken.
//
// Sequence is per epoch, strictly increasing and gapless: a lost run of
// events occupies one ProcessDropped sequence carrying their count. Only the
// contained workload's processes are recorded, the login shell included,
// never the guest helper or init. A process that forks without starting a
// program is part of its parent and is not recorded. A process that starts
// another program in place reports another start under the same PID, and
// its one exit ends the latest. An exit never precedes its start within an
// epoch, but PIDs are reused, so pair starts and exits by order. An exit
// whose start was lost follows the ProcessDropped event that counts it.
type ProcessEvent struct {
	Sequence uint64      `json:"sequence"`
	Kind     ProcessKind `json:"kind"`
	// PID is the guest process id (started and exited).
	PID int `json:"pid,omitempty"`
	// ParentPID is the guest parent process id when it was known (started).
	ParentPID int `json:"parent_pid,omitempty"`
	// Argv is the command line (started), bounded by MaxProcessArgs and
	// MaxProcessArgvBytes. Invalid UTF-8 is replaced.
	Argv []string `json:"argv,omitempty"`
	// Truncated reports that Argv was cut to its bounds (started).
	Truncated bool `json:"truncated,omitempty"`
	// Unread reports that the process ended before its command line and
	// working directory could be read (started). Argv and Dir are empty.
	Unread bool `json:"unread,omitempty"`
	// Dir is the working directory when it was readable and inside the
	// workspace or the scratch home, else "" (started).
	Dir string `json:"dir,omitempty"`
	// At is the guest clock when the event was observed. For a dropped
	// event it is when the loss was reported.
	At time.Time `json:"at"`
	// ExitCode is the exit status, or nil when a signal ended the process
	// (exited).
	ExitCode *int `json:"exit_code,omitempty"`
	// Signal is the terminating signal, or 0 (exited).
	Signal int `json:"signal,omitempty"`
	// Dropped is how many events were lost before this one (dropped). It is
	// exact when the host's retention overflowed. When the guest kernel's
	// own event buffer overflowed the number is unknown and each such
	// overflow counts as 1.
	Dropped uint64 `json:"dropped,omitempty"`
}

// ProcessPage is one page of the process log. Next is the sequence to ask
// after for the following page: the last event's sequence, or the
// requested position when the page is empty.
type ProcessPage struct {
	Events []ProcessEvent `json:"events"`
	Next   uint64         `json:"next"`
}

// ValidateProcessEvent checks one sequenced process log entry against its
// kind and the documented bounds.
func ValidateProcessEvent(event ProcessEvent) error {
	if event.Sequence == 0 || event.Sequence > MaxProjectionCursor {
		return ErrInvalid
	}
	switch event.Kind {
	case ProcessStarted:
		if !validPID(event.PID) || event.ParentPID < 0 || event.ParentPID > maxProcessPID || event.ExitCode != nil || event.Signal != 0 || event.Dropped != 0 {
			return ErrInvalid
		}
		if event.Unread && (len(event.Argv) != 0 || event.Truncated || event.Dir != "") {
			return ErrInvalid
		}
		if len(event.Argv) > MaxProcessArgs {
			return ErrInvalid
		}
		total := 0
		for _, arg := range event.Argv {
			if !utf8.ValidString(arg) {
				return ErrInvalid
			}
			total += len(arg)
		}
		if total > MaxProcessArgvBytes {
			return ErrInvalid
		}
		if event.Dir != "" && (len(event.Dir) > MaxProcessDirBytes || !utf8.ValidString(event.Dir) || !path.IsAbs(event.Dir) || path.Clean(event.Dir) != event.Dir) {
			return ErrInvalid
		}
	case ProcessExited:
		if !validPID(event.PID) || event.ParentPID != 0 || len(event.Argv) != 0 || event.Truncated || event.Unread || event.Dir != "" || event.Dropped != 0 {
			return ErrInvalid
		}
		if (event.ExitCode == nil) == (event.Signal == 0) {
			return ErrInvalid
		}
		if event.ExitCode != nil && (*event.ExitCode < 0 || *event.ExitCode > 255) {
			return ErrInvalid
		}
		if event.Signal < 0 || event.Signal > 127 {
			return ErrInvalid
		}
	case ProcessDropped:
		if event.Dropped == 0 || event.PID != 0 || event.ParentPID != 0 || len(event.Argv) != 0 || event.Truncated || event.Unread || event.Dir != "" || event.ExitCode != nil || event.Signal != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func validPID(pid int) bool { return pid > 0 && pid <= maxProcessPID }
