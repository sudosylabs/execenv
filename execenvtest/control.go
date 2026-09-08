package execenvtest

import (
	"errors"
	"github.com/sudosylabs/execenv"
	"testing"
)

func runControl(t *testing.T, factory Factory) {
	t.Run("opaque control fences survive reensure and reject replacement effects", func(t *testing.T) {
		host := factory(t)
		spec := execenv.Spec{ID: "fenced-grant", Image: "default"}
		env, err := host.Ensure(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		controlled, ok := env.(execenv.ControlledEnv)
		if !ok {
			t.Skip("adapter has no controlled epochs")
		}
		t.Cleanup(func() { _ = host.Revoke(t.Context(), spec.ID) })
		req := execenv.ControlRequest{Fence: execenv.ProjectionFence{ExecutionGrantID: spec.ID, EnvironmentEpoch: controlled.Epoch(), FenceRevision: 2}, State: execenv.ControlFrozen}
		receipt, err := controlled.Control(t.Context(), req)
		if err != nil || !receipt.Confirmed || receipt.Fence != req.Fence {
			t.Fatalf("freeze %+v %v", receipt, err)
		}
		again, err := host.Ensure(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		current := again.(execenv.ControlledEnv)
		if current.Epoch() != controlled.Epoch() {
			t.Fatal("reensure changed live epoch")
		}
		stale := req
		stale.State = execenv.ControlRunning
		stale.Fence.FenceRevision = 1
		if _, err := current.Control(t.Context(), stale); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("stale thaw: %v", err)
		}
		if err := env.Thaw(t.Context()); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("unfenced thaw: %v", err)
		}
		req.Fence.FenceRevision++
		req.State = execenv.ControlRunning
		if receipt, err = current.Control(t.Context(), req); err != nil || !receipt.Confirmed {
			t.Fatalf("thaw %+v %v", receipt, err)
		}
		req.Fence.FenceRevision++
		req.State = execenv.ControlRevoked
		if receipt, err = current.Control(t.Context(), req); err != nil || !receipt.Confirmed {
			t.Fatalf("revoke %+v %v", receipt, err)
		}
		successor, err := host.Ensure(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.ReplaceTree(t.Context(), execenv.Tree{{Path: "stale", Kind: execenv.KindFile, Data: []byte("stale")}}); err == nil {
			t.Fatal("stale projection accepted")
		}
		if successor.(execenv.ControlledEnv).Epoch() == current.Epoch() {
			t.Fatal("epoch reused")
		}
		if err := env.Revoke(t.Context()); err != nil && !errors.Is(err, execenv.ErrConflict) {
			t.Fatal(err)
		}
		if _, err := successor.Attach(t.Context(), execenv.Window{}); err != nil {
			t.Fatalf("stale revoke affected successor: %v", err)
		}
		req.State = execenv.ControlRunning
		req.Fence.FenceRevision++
		if _, err := successor.(execenv.ControlledEnv).Control(t.Context(), req); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("old epoch command: %v", err)
		}
	})
}
