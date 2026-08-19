package mux

import (
	"context"
	"errors"

	"github.com/sudosylabs/execenv"
)

// StatusOf maps an error to a stable name. Unknown errors become
// "unavailable" so adapter internals never cross the wire.
func StatusOf(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, execenv.ErrInvalid):
		return "invalid"
	case errors.Is(err, execenv.ErrUnknownImage):
		return "unknown_image"
	case errors.Is(err, execenv.ErrCapacity):
		return "capacity"
	case errors.Is(err, execenv.ErrConflict):
		return "conflict"
	case errors.Is(err, execenv.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, execenv.ErrFrozen):
		return "frozen"
	case errors.Is(err, execenv.ErrRevoked):
		return "revoked"
	case errors.Is(err, execenv.ErrBusy):
		return "busy"
	case errors.Is(err, execenv.ErrClosed):
		return "closed"
	case errors.Is(err, execenv.ErrNotFound):
		return "not_found"
	case errors.Is(err, execenv.ErrLagged):
		return "lagged"
	case errors.Is(err, execenv.ErrTooLarge):
		return "too_large"
	case errors.Is(err, execenv.ErrConnection):
		return "connection"
	case errors.Is(err, execenv.ErrNetwork):
		return "network"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "unavailable"
	}
}

// ErrorFromStatus is the inverse of StatusOf.
func ErrorFromStatus(status string) error {
	switch status {
	case "":
		return nil
	case "invalid":
		return execenv.ErrInvalid
	case "unknown_image":
		return execenv.ErrUnknownImage
	case "capacity":
		return execenv.ErrCapacity
	case "conflict":
		return execenv.ErrConflict
	case "unavailable":
		return execenv.ErrUnavailable
	case "frozen":
		return execenv.ErrFrozen
	case "revoked":
		return execenv.ErrRevoked
	case "busy":
		return execenv.ErrBusy
	case "closed":
		return execenv.ErrClosed
	case "not_found":
		return execenv.ErrNotFound
	case "lagged":
		return execenv.ErrLagged
	case "too_large":
		return execenv.ErrTooLarge
	case "connection":
		return execenv.ErrConnection
	case "network":
		return execenv.ErrNetwork
	case "canceled":
		return context.Canceled
	default:
		return execenv.ErrUnavailable
	}
}
