package memory

import (
	"context"
	"github.com/sudosylabs/execenv"
)

var _ execenv.ControlledEnv = (*environment)(nil)

func (e *environment) Epoch() string { return e.control.Epoch() }
func (e *environment) ControlStatus(ctx context.Context) (execenv.ControlReceipt, error) {
	return e.control.Status(ctx)
}
func (e *environment) Control(ctx context.Context, request execenv.ControlRequest) (execenv.ControlReceipt, error) {
	receipt, err := e.control.Execute(ctx, request, func() error {
		e.mu.Lock()
		e.projectionFence = request.Fence
		e.mu.Unlock()
		switch request.State {
		case execenv.ControlFrozen:
			return e.freeze(ctx)
		case execenv.ControlRunning:
			return e.thaw(ctx)
		case execenv.ControlRevoked:
			return e.stop()
		default:
			return execenv.ErrInvalid
		}
	})
	if err == nil && request.State == execenv.ControlRevoked {
		err = e.host.revokeExact(ctx, e)
	}
	return receipt, err
}
