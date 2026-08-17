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
	// ErrCapacity indicates the host has no remaining grant slots.
	ErrCapacity = errors.New("execenv: no capacity")
	// ErrConflict indicates a live grant was ensured with a different image or network.
	ErrConflict = errors.New("execenv: grant conflict")
	// ErrUnavailable indicates the host cannot occupy grants.
	ErrUnavailable = errors.New("execenv: host unavailable")
	// ErrFrozen indicates the grant is frozen.
	ErrFrozen = errors.New("execenv: grant frozen")
	// ErrRevoked indicates the grant no longer exists.
	ErrRevoked = errors.New("execenv: grant revoked")
	// ErrBusy indicates a terminal is already attached.
	ErrBusy = errors.New("execenv: terminal busy")
	// ErrClosed indicates the terminal or observation has been closed.
	ErrClosed = errors.New("execenv: closed")
	// ErrNotFound indicates a path does not exist in the projection.
	ErrNotFound = errors.New("execenv: not found")
	// ErrLagged indicates Watch dropped events and the caller must resync.
	ErrLagged = errors.New("execenv: observation lagged")
	// ErrTooLarge indicates a file or tree exceeds the documented limits.
	ErrTooLarge = errors.New("execenv: too large")
	// ErrConnection indicates the remote session was lost. A dropped PTY is
	// hangup (ErrClosed), not this error, and does not revoke the grant.
	ErrConnection = errors.New("execenv: connection failed")
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
