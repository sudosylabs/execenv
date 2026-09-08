// Package control serializes the generic grant/epoch control contract.
package control

import (
	"context"
	"crypto/rand"
	"github.com/sudosylabs/execenv"
)

// State owns one epoch's highest intent and acknowledgement. Adapters retain
// this value for the complete epoch lifetime, including across remote sessions.
type State struct {
	lock    chan struct{}
	epoch   string
	receipt execenv.ControlReceipt
}

func New(id execenv.ID) *State {
	epoch := rand.Text()
	return &State{lock: make(chan struct{}, 1), epoch: epoch, receipt: execenv.ControlReceipt{Fence: execenv.ProjectionFence{ExecutionGrantID: id, EnvironmentEpoch: epoch}, State: execenv.ControlRunning, Confirmed: true}}
}

func (s *State) Epoch() string { return s.epoch }

func (s *State) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.lock <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		<-s.lock
		return err
	}
	return nil
}
func (s *State) release() { <-s.lock }

func (s *State) Status(ctx context.Context) (execenv.ControlReceipt, error) {
	if err := s.acquire(ctx); err != nil {
		return execenv.ControlReceipt{}, err
	}
	defer s.release()
	return s.receipt, nil
}

// Execute records intent before performing I/O. A timeout or failed effect can
// be retried with exactly the same request, but cannot reopen an older fence.
func (s *State) Execute(ctx context.Context, req execenv.ControlRequest, effect func() error) (execenv.ControlReceipt, error) {
	if err := execenv.ValidateControl(req); err != nil {
		return execenv.ControlReceipt{}, err
	}
	if err := s.acquire(ctx); err != nil {
		return execenv.ControlReceipt{}, err
	}
	defer s.release()
	old := s.receipt
	if req.Fence.ExecutionGrantID != old.Fence.ExecutionGrantID || req.Fence.EnvironmentEpoch != s.epoch {
		return old, execenv.ErrConflict
	}
	if old.State == execenv.ControlRevoked && (old.Fence != req.Fence || req.State != old.State) {
		return old, execenv.ErrRevoked
	}
	if req.Fence.FenceRevision < old.Fence.FenceRevision || (req.Fence.FenceRevision == old.Fence.FenceRevision && req.State != old.State) {
		return old, execenv.ErrConflict
	}
	if req.Fence == old.Fence && old.Confirmed {
		return old, nil
	}
	s.receipt = execenv.ControlReceipt{Fence: req.Fence, State: req.State}
	err := effect()
	s.receipt.Confirmed = err == nil
	return s.receipt, err
}

// Legacy serializes old control calls and refuses their use after an epoch has
// entered the fenced protocol. Revoke remains available as a fail-closed action.
func (s *State) Legacy(ctx context.Context, state execenv.ControlState, effect func() error) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.receipt.State == execenv.ControlRevoked && state != execenv.ControlRevoked {
		return execenv.ErrRevoked
	}
	if state != execenv.ControlRevoked && s.receipt.Fence.FenceRevision != 0 {
		return execenv.ErrConflict
	}
	if state == execenv.ControlRevoked && s.receipt.State == state && s.receipt.Confirmed {
		return nil
	}
	s.receipt.State, s.receipt.Confirmed = state, false
	err := effect()
	s.receipt.Confirmed = err == nil
	return err
}

// Run holds the same serialization boundary as control effects. A matching
// request may use only a confirmed running epoch; a rejected fence performs no I/O.
func (s *State) Run(ctx context.Context, fence execenv.ProjectionFence, effect func() error) error {
	if err := execenv.ValidateProjectionFence(fence); err != nil {
		return err
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.receipt.State == execenv.ControlRevoked {
		return execenv.ErrRevoked
	}
	if s.receipt.Fence != fence {
		return execenv.ErrConflict
	}
	if !s.receipt.Confirmed || s.receipt.State != execenv.ControlRunning {
		return execenv.ErrFrozen
	}
	return effect()
}
