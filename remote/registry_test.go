package remote

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/sudosylabs/execenv"
)

type retiringEnv struct {
	execenv.Env
	epoch   string
	retired chan struct{}
}

func (e *retiringEnv) ID() execenv.ID           { return "grant" }
func (e *retiringEnv) Epoch() string            { return e.epoch }
func (e *retiringEnv) Retired() <-chan struct{} { return e.retired }

func (e *retiringEnv) ControlStatus(context.Context) (execenv.ControlReceipt, error) {
	return execenv.ControlReceipt{
		Fence: execenv.ProjectionFence{ExecutionGrantID: e.ID(), EnvironmentEpoch: e.epoch, FenceRevision: 2},
		State: execenv.ControlRevoked, Confirmed: true,
	}, nil
}

func TestRegistryForgetsOnlyTheRetiredEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := make(chan struct{})
		defer close(done)
		h := &server{envs: map[execenv.ID]execenv.Env{}, done: done}
		old := &retiringEnv{epoch: "old", retired: make(chan struct{})}
		current := &retiringEnv{epoch: "current", retired: make(chan struct{})}
		h.remember(old)
		h.remember(current)
		close(old.retired)
		synctest.Wait()
		if got, err := h.env(current.ID(), current.Epoch()); got != current || err != nil {
			t.Fatalf("retirement removed a replacement: %v", err)
		}
		close(current.retired)
		synctest.Wait()
		if _, err := h.env(current.ID(), current.Epoch()); err != execenv.ErrRevoked {
			t.Fatalf("retired epoch is still registered: %v", err)
		}
		if receipt, ok := h.controlReceipt(current.ID(), current.Epoch()); !ok || receipt.State != execenv.ControlRevoked {
			t.Fatalf("retirement dropped the control receipt: %+v", receipt)
		}
	})
}
