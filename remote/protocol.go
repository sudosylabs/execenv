package remote

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"

	"github.com/sudosylabs/execenv"
)

// frameKind is the only on-wire discriminant. Control RPCs, PTY octets, and
// Watch events share one connection so closing a PTY is hangup, not a TCP
// teardown and not a revoke.
type frameKind uint8

const (
	kindRequest frameKind = iota + 1
	kindResponse
	kindPty
	kindWatch
)

// frame is the only encoded value. Status is a sentinel name, never a free
// text error, so PTY octets and file bodies cannot leak through Error().
type frame struct {
	Seq    uint64
	Kind   frameKind
	Method string
	Grant  string
	Status string
	Extra  []byte
}

const (
	methodAuth    = "auth"
	methodReady   = "ready"
	methodEnsure  = "ensure"
	methodRevoke  = "revoke"
	methodAttach  = "attach"
	methodDetach  = "detach"
	methodResize  = "resize"
	methodFreeze  = "freeze"
	methodThaw    = "thaw"
	methodReplace = "replace"
	methodApply   = "apply"
	methodWatch   = "watch"
	methodUnwatch = "unwatch"
	methodOpen    = "open"
)

type authArgs struct {
	Token []byte
}

type ensureArgs struct {
	Spec execenv.Spec
}

type replaceArgs struct {
	Tree execenv.Tree
}

type applyArgs struct {
	Batch execenv.Batch
}

type openArgs struct {
	Path string
}

type attachArgs struct {
	Window execenv.Window
}

type resizeArgs struct {
	Window execenv.Window
}

func encodeExtra(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeExtra(data []byte, dest any) error {
	if len(data) == 0 {
		return nil
	}
	return gob.NewDecoder(bytes.NewReader(data)).Decode(dest)
}

// statusOf maps an error to a stable name. Unknown errors become
// "unavailable" so adapter internals never cross the wire.
func statusOf(err error) string {
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

func errorFromStatus(status string) error {
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
