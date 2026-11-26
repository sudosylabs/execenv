// Package control serializes the generic grant/epoch control contract.
package control

import (
	"context"
	"crypto/rand"
	"sync/atomic"

	"github.com/sudosylabs/execenv"
)

// State owns one epoch's highest intent and acknowledgement. Adapters retain
// this value for the complete epoch lifetime, including across remote sessions.
type State struct {
	lock    chan struct{}
	epoch   string
	receipt execenv.ControlReceipt
	// lost is set when the environment ended without a revoke. The epoch is
	// then terminal like a revoke, but every fenced call reports ErrLost so
	// the caller can tell a host-side end from its own revoke.
	lost bool
	// seen is the receipt as the last operation left it, for readers that
	// must not wait for one under way.
	seen atomic.Pointer[seen]
	// changed, when set, is told after every operation.
	changed atomic.Pointer[func()]
	// save persists the intent and confirmation under the control lock.
	save func(execenv.ControlReceipt) error
}

type seen struct {
	receipt execenv.ControlReceipt
	lost    bool
}

func New(id execenv.ID) *State {
	epoch := rand.Text()
	s := &State{lock: make(chan struct{}, 1), epoch: epoch, receipt: execenv.ControlReceipt{Fence: execenv.ProjectionFence{ExecutionGrantID: id, EnvironmentEpoch: epoch}, State: execenv.ControlRunning, Confirmed: true}}
	s.seen.Store(&seen{receipt: s.receipt})
	return s
}

// Restore resumes an epoch a restarted host recorded, with its last
// receipt.
func Restore(epoch string, receipt execenv.ControlReceipt) *State {
	s := &State{lock: make(chan struct{}, 1), epoch: epoch, receipt: receipt}
	s.seen.Store(&seen{receipt: receipt})
	return s
}

// Peek is the receipt as the last control left it, and whether the epoch
// was lost, without waiting for one under way.
func (s *State) Peek() (execenv.ControlReceipt, bool) {
	v := s.seen.Load()
	return v.receipt, v.lost
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
func (s *State) release() {
	s.seen.Store(&seen{receipt: s.receipt, lost: s.lost})
	<-s.lock
	if f := s.changed.Load(); f != nil {
		(*f)()
	}
}

// OnChange has f told after every operation, once its lock is released.
func (s *State) OnChange(f func()) { s.changed.Store(&f) }

// SetPersistence installs the durable writer before the state is published.
func (s *State) SetPersistence(save func(execenv.ControlReceipt) error) { s.save = save }

func (s *State) transition(next execenv.ControlReceipt, effect func() error) error {
	if s.save != nil {
		if err := s.save(next); err != nil {
			return err
		}
	}
	s.receipt = next
	if err := effect(); err != nil {
		return err
	}
	next.Confirmed = true
	if s.save != nil {
		if err := s.save(next); err != nil {
			return err
		}
	}
	s.receipt = next
	return nil
}

func (s *State) Status(ctx context.Context) (execenv.ControlReceipt, error) {
	if err := s.acquire(ctx); err != nil {
		return execenv.ControlReceipt{}, err
	}
	defer s.release()
	if s.lost {
		return s.receipt, execenv.ErrLost
	}
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
	if s.lost {
		return old, execenv.ErrLost
	}
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
	err := s.transition(execenv.ControlReceipt{Fence: req.Fence, State: req.State}, effect)
	return s.receipt, err
}

// Revoke serializes an unfenced, terminal revoke with fenced control. It
// records the epoch as revoked before destruction, so a failed effect still
// fences every later request and can only be retried as a revoke.
func (s *State) Revoke(ctx context.Context, effect func() error) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.receipt.State == execenv.ControlRevoked && s.receipt.Confirmed {
		return nil
	}
	next := s.receipt
	next.State, next.Confirmed = execenv.ControlRevoked, false
	return s.transition(next, effect)
}

// Lose ends the epoch because its environment ended without a revoke. Like
// Revoke it is terminal and fences every later request, which then fails
// with ErrLost. It reports false, and runs nothing, when the epoch was
// already revoked or lost; a failed effect can be retried with Revoke.
func (s *State) Lose(ctx context.Context, effect func() error) (bool, error) {
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()
	if s.lost || s.receipt.State == execenv.ControlRevoked {
		return false, nil
	}
	s.lost = true
	s.receipt.State, s.receipt.Confirmed = execenv.ControlRevoked, false
	err := effect()
	s.receipt.Confirmed = err == nil
	return true, err
}

// Idle freezes a running epoch on the host's own initiative, for its idle
// timeout. It keeps the caller's fence revision: the caller sees frozen in
// ControlStatus and thaws with its next revision, as after its own freeze.
// It reports false, and runs nothing, unless the epoch is confirmed running.
func (s *State) Idle(ctx context.Context, effect func(done execenv.ControlReceipt) error) (bool, error) {
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()
	if s.lost || s.receipt.State != execenv.ControlRunning || !s.receipt.Confirmed {
		return false, nil
	}
	next := s.receipt
	next.State, next.Confirmed = execenv.ControlFrozen, false
	// done is what the grant's control records once effect succeeds, for an
	// effect that must make it durable before it finishes.
	done := next
	done.Confirmed = true
	return true, s.transition(next, func() error { return effect(done) })
}

// Suspend saves a confirmed frozen epoch on the host's own initiative, for
// its idle timeout. The receipt does not change: the epoch stays frozen
// under the caller's fence, which thaws it with its next revision as
// before, and a suspend that fails leaves it frozen. effect is given that
// receipt. It reports false, and runs nothing, unless the epoch is
// confirmed frozen.
func (s *State) Suspend(ctx context.Context, effect func(done execenv.ControlReceipt) error) (bool, error) {
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()
	if s.lost || s.receipt.State != execenv.ControlFrozen || !s.receipt.Confirmed {
		return false, nil
	}
	return true, effect(s.receipt)
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
	if s.lost {
		return execenv.ErrLost
	}
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
