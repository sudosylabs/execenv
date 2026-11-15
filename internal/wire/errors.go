package wire

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
)

// sentinels maps every execenv error to the name it carries on the wire and
// the Connect code a caller in any language sees.
var sentinels = []struct {
	err  error
	name string
	code connect.Code
}{
	{execenv.ErrInvalid, "invalid", connect.CodeInvalidArgument},
	{execenv.ErrUnknownImage, "unknown_image", connect.CodeFailedPrecondition},
	{execenv.ErrCapacity, "capacity", connect.CodeResourceExhausted},
	{execenv.ErrDraining, "draining", connect.CodeUnavailable},
	{execenv.ErrLost, "lost", connect.CodeFailedPrecondition},
	{execenv.ErrConflict, "conflict", connect.CodeAborted},
	{execenv.ErrUnavailable, "unavailable", connect.CodeUnavailable},
	{execenv.ErrFrozen, "frozen", connect.CodeFailedPrecondition},
	{execenv.ErrRevoked, "revoked", connect.CodeFailedPrecondition},
	{execenv.ErrBusy, "busy", connect.CodeResourceExhausted},
	{execenv.ErrSuperseded, "superseded", connect.CodeAborted},
	{execenv.ErrClosed, "closed", connect.CodeFailedPrecondition},
	{execenv.ErrNotFound, "not_found", connect.CodeNotFound},
	{execenv.ErrLagged, "lagged", connect.CodeFailedPrecondition},
	{execenv.ErrTooLarge, "too_large", connect.CodeResourceExhausted},
	{execenv.ErrConnection, "connection", connect.CodeUnavailable},
	{execenv.ErrNetwork, "network", connect.CodeFailedPrecondition},
	{execenv.ErrFeature, "feature", connect.CodeFailedPrecondition},
	{execenv.ErrQuota, "quota", connect.CodeResourceExhausted},
}

// wireError turns a host error into a Connect error. Its message is the
// sentinel's own text: host errors never carry file bodies, PTY bytes or
// tokens, and nothing else of the inner error crosses the wire.
func Error(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	for _, s := range sentinels {
		if errors.Is(err, s.err) {
			detail := &execenvv1.ErrorDetail{Sentinel: s.name}
			var short *execenv.CapacityError
			if errors.As(err, &short) {
				detail.Shortfall = &execenvv1.Shortfall{Resource: resources[short.Resource], Requested: short.Requested, Free: short.Free}
			}
			return WithDetail(connect.NewError(s.code, s.err), detail)
		}
	}
	return WithDetail(connect.NewError(connect.CodeUnavailable, execenv.ErrUnavailable), &execenvv1.ErrorDetail{Sentinel: "unavailable"})
}

var resources = map[execenv.Resource]execenvv1.Resource{
	execenv.ResourceCPU:    execenvv1.Resource_RESOURCE_CPU,
	execenv.ResourceMemory: execenvv1.Resource_RESOURCE_MEMORY,
	execenv.ResourceDisk:   execenvv1.Resource_RESOURCE_DISK,
}

func shortfallFrom(s *execenvv1.Shortfall) error {
	for k, v := range resources {
		if v == s.GetResource() {
			return &execenv.CapacityError{Resource: k, Requested: s.GetRequested(), Free: s.GetFree()}
		}
	}
	return execenv.ErrCapacity
}

func WithDetail(err *connect.Error, detail *execenvv1.ErrorDetail) *connect.Error {
	if d, derr := connect.NewErrorDetail(detail); derr == nil {
		err.AddDetail(d)
	}
	return err
}

// hostError turns a wire error back into the execenv sentinel. A response
// without a detail is not from an execenv host, or the connection failed.
func HostError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return execenv.Error(op, err)
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return execenv.Error(op, execenv.ErrConnection)
	}
	for _, d := range cerr.Details() {
		value, verr := d.Value()
		detail, ok := value.(*execenvv1.ErrorDetail)
		if verr != nil || !ok {
			continue
		}
		if detail.GetSentinel() == "capacity" && detail.GetShortfall() != nil {
			return execenv.Error(op, shortfallFrom(detail.GetShortfall()))
		}
		for _, s := range sentinels {
			if s.name == detail.GetSentinel() {
				return execenv.Error(op, s.err)
			}
		}
	}
	switch cerr.Code() {
	case connect.CodeCanceled:
		return execenv.Error(op, context.Canceled)
	case connect.CodeDeadlineExceeded:
		return execenv.Error(op, context.DeadlineExceeded)
	case connect.CodeUnauthenticated:
		return execenv.Error(op, execenv.ErrUnavailable)
	default:
		return execenv.Error(op, execenv.ErrConnection)
	}
}
