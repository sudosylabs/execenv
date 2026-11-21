package execenv

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalid indicates a grant id, image, or network value cannot be used.
	ErrInvalid = errors.New("execenv: invalid")
	// ErrUnknownImage indicates the host does not have the requested image.
	ErrUnknownImage = errors.New("execenv: unknown image")
	// ErrCapacity indicates the host has no room for the grant's resources.
	// A *CapacityError says which resource was short.
	ErrCapacity = errors.New("execenv: no capacity")
	// ErrDraining indicates the host's operator is draining it before a
	// restart: it serves the grants it holds but occupies no new ones.
	// Ensure of a grant the host already holds still returns it. A caller
	// placing a new grant chooses another host, as for ErrCapacity, until
	// Ready reports the host Usable again.
	ErrDraining = errors.New("execenv: host draining")
	// ErrConflict indicates a call that disagrees with the grant's current
	// state: a stale fence, a live grant ensured with another spec, a file
	// that is not the expected one, or an input or transfer offset that is
	// not where the host stands.
	ErrConflict = errors.New("execenv: grant conflict")
	// ErrUnavailable indicates the host cannot occupy grants.
	ErrUnavailable = errors.New("execenv: host unavailable")
	// ErrFrozen indicates the grant is frozen.
	ErrFrozen = errors.New("execenv: grant frozen")
	// ErrRevoked indicates the grant no longer exists.
	ErrRevoked = errors.New("execenv: grant revoked")
	// ErrLost indicates the grant's environment ended without a revoke:
	// its machine stopped, its guest crashed, or the host lost the guest
	// link for good. The host has already released every resource it held, and every call on that occupancy fails with
	// ErrLost. Ensure of the same id also fails with ErrLost until the
	// caller acknowledges the loss with Revoke; after that Ensure occupies
	// the id afresh under a new Epoch. The caller rebuilds from its own
	// journal, as after any new occupancy.
	ErrLost = errors.New("execenv: grant lost")
	// ErrBusy indicates a grant already runs MaxProcesses processes.
	ErrBusy = errors.New("execenv: too many processes")
	// ErrRefused indicates no listener accepted the loopback connection.
	ErrRefused = errors.New("execenv: port connection refused")
	// ErrClosed indicates the process, attachment or reader has ended.
	ErrClosed = errors.New("execenv: closed")
	// ErrNotFound indicates a grant, path, process, seed or transfer that
	// does not exist.
	ErrNotFound = errors.New("execenv: not found")
	// ErrLagged indicates the host can no longer report guest changes from
	// the requested position, or could not capture one. The caller must
	// resynchronize from its own journal. For the process log it means the
	// requested position was already acknowledged.
	ErrLagged = errors.New("execenv: observation lagged")
	// ErrStale indicates an observed file changed after it was observed, so
	// its content can no longer be read as observed. A later observation of
	// the same entry reports the change: skip this one and read on.
	ErrStale = errors.New("execenv: observation content changed")
	// ErrTooLarge indicates metadata over its documented bound, or too many
	// content handles.
	ErrTooLarge = errors.New("execenv: too large")
	// ErrConnection indicates the remote session was lost. The host treats
	// it as a detach: the grant and its processes stay, and a new connection
	// attaches again from the output offsets it had.
	ErrConnection = errors.New("execenv: connection failed")
	// ErrNetwork indicates the grant requested a network the host does not
	// offer. The caller cannot supply destinations; only the host allowlist
	// exists, and it may be empty.
	ErrNetwork = errors.New("execenv: network not offered")
	// ErrFeature indicates the host does not offer a feature the caller
	// needs. The client asks for its features when it connects, so a host
	// too old or too new for it is refused at once, not halfway through.
	ErrFeature = errors.New("execenv: feature not offered")
	// ErrQuota indicates the grant's workspace disk is full. Nothing is
	// truncated: the call that hit it changes nothing.
	ErrQuota = errors.New("execenv: disk quota exceeded")
	// ErrMissingContent indicates an initialization names file bodies that
	// were neither uploaded nor in the grant's seed. errors.As a
	// *MissingContentError for the paths.
	ErrMissingContent = errors.New("execenv: missing content")
)

// OpError adds operation context while preserving errors.Is matching.
type OpError struct {
	Op  string
	Err error
}

func (e *OpError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("execenv %s: %v", e.Op, e.Err)
}

func (e *OpError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Error annotates err with an operation. A nil error remains nil.
func Error(op string, err error) error {
	if err == nil {
		return nil
	}
	return &OpError{Op: op, Err: err}
}
