package remote

import (
	"context"
	"github.com/sudosylabs/execenv"
)

var _ execenv.ControlledEnv = (*remoteEnv)(nil)

func (e *remoteEnv) Epoch() string { return e.epoch }
func (e *remoteEnv) Control(ctx context.Context, request execenv.ControlRequest) (execenv.ControlReceipt, error) {
	if request.Fence.ExecutionGrantID != e.id || request.Fence.EnvironmentEpoch != e.epoch {
		return execenv.ControlReceipt{}, execenv.ErrConflict
	}
	if err := execenv.ValidateControl(request); err != nil {
		return execenv.ControlReceipt{}, err
	}
	extra, err := encodeExtra(request)
	if err != nil {
		return execenv.ControlReceipt{}, err
	}
	if request.State != execenv.ControlRunning {
		e.setTermFrozen(true)
	}
	out, err := e.client.call(ctx, methodControl, string(e.id), extra)
	if err != nil {
		return execenv.ControlReceipt{}, err
	}
	var receipt execenv.ControlReceipt
	if err := decodeExtra(out, &receipt); err != nil || receipt.Fence != request.Fence || receipt.State != request.State || !receipt.Confirmed {
		return execenv.ControlReceipt{}, execenv.ErrUnavailable
	}
	if request.State == execenv.ControlRevoked {
		e.revoked.Store(true)
		e.hangupTerm(execenv.ErrRevoked)
	} else {
		e.setTermFrozen(request.State == execenv.ControlFrozen)
	}
	return receipt, nil
}

func (e *remoteEnv) ControlStatus(ctx context.Context) (execenv.ControlReceipt, error) {
	extra, err := encodeExtra(e.epoch)
	if err != nil {
		return execenv.ControlReceipt{}, err
	}
	out, err := e.client.call(ctx, methodControlStatus, string(e.id), extra)
	if err != nil {
		return execenv.ControlReceipt{}, err
	}
	var receipt execenv.ControlReceipt
	if err := decodeExtra(out, &receipt); err != nil || receipt.Fence.ExecutionGrantID != e.id || receipt.Fence.EnvironmentEpoch != e.epoch {
		return execenv.ControlReceipt{}, execenv.ErrUnavailable
	}
	return receipt, nil
}

func (sc *serverConn) control(ctx context.Context, f frame) ([]byte, error) {
	var request execenv.ControlRequest
	if err := decodeExtra(f.Extra, &request); err != nil || string(request.Fence.ExecutionGrantID) != f.Grant {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return nil, err
	}
	controlled, ok := env.(execenv.ControlledEnv)
	if !ok {
		return nil, execenv.ErrUnavailable
	}
	receipt, err := controlled.Control(ctx, request)
	if err != nil {
		return nil, err
	}
	return encodeExtra(receipt)
}

func (sc *serverConn) controlStatus(ctx context.Context, f frame) ([]byte, error) {
	var epoch string
	if err := decodeExtra(f.Extra, &epoch); err != nil {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return nil, err
	}
	controlled, ok := env.(execenv.ControlledEnv)
	if !ok {
		return nil, execenv.ErrUnavailable
	}
	if controlled.Epoch() != epoch {
		return nil, execenv.ErrConflict
	}
	receipt, err := controlled.ControlStatus(ctx)
	if err != nil {
		return nil, err
	}
	return encodeExtra(receipt)
}

func (sc *serverConn) revokeEpoch(ctx context.Context, f frame) error {
	var epoch string
	if err := decodeExtra(f.Extra, &epoch); err != nil || epoch == "" {
		return execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return err
	}
	controlled, ok := env.(execenv.ControlledEnv)
	if !ok {
		return execenv.ErrUnavailable
	}
	if controlled.Epoch() != epoch {
		return execenv.ErrConflict
	}
	return env.Revoke(ctx)
}

// All legacy environment effects still bind the captured epoch. Terminal
// frames separately bind the connection's unique stream identity.
type epochArgs struct {
	Epoch string
	Body  []byte
}

func (e *remoteEnv) call(ctx context.Context, method, grant string, body []byte) ([]byte, error) {
	if e.revoked.Load() {
		return nil, execenv.ErrRevoked
	}
	extra, err := encodeExtra(epochArgs{Epoch: e.epoch, Body: body})
	if err != nil {
		return nil, err
	}
	return e.client.call(ctx, method, grant, extra)
}
func epochMethod(method string) bool {
	switch method {
	case methodAttach, methodReplace, methodApply, methodWatch, methodOpen, methodFreeze, methodThaw:
		return true
	default:
		return false
	}
}
func (sc *serverConn) unwrapEpoch(f *frame) error {
	var args epochArgs
	if err := decodeExtra(f.Extra, &args); err != nil {
		return execenv.ErrInvalid
	}
	env, err := sc.env(execenv.ID(f.Grant))
	if err != nil {
		return err
	}
	if controlled, ok := env.(execenv.ControlledEnv); ok {
		if args.Epoch != controlled.Epoch() {
			return execenv.ErrConflict
		}
	} else if args.Epoch != "" {
		return execenv.ErrConflict
	}
	f.Extra = args.Body
	return nil
}
func (e *remoteEnv) hangupTerm(err error) {
	e.client.mu.Lock()
	term := e.client.terms[e.id]
	if term != nil && term.epoch == e.epoch {
		delete(e.client.terms, e.id)
	} else {
		term = nil
	}
	e.client.mu.Unlock()
	if term != nil {
		term.hangup(err)
	}
}
