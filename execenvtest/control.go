package execenvtest

import (
	"errors"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

func runControl(t *testing.T, factory Factory) {
	t.Run("opaque control fences survive reensure and reject replacement effects", func(t *testing.T) {
		host := factory(t)
		spec := execenv.Spec{ID: "fenced-grant", Image: "default"}
		env, err := host.Ensure(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		controlled := env
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
		current := again
		if current.Epoch() != controlled.Epoch() {
			t.Fatal("reensure changed live epoch")
		}
		stale := req
		stale.State = execenv.ControlRunning
		stale.Fence.FenceRevision = 1
		if _, err := current.Control(t.Context(), stale); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("stale thaw: %v", err)
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
		if _, err := env.UploadProjectionContent(t.Context(), req.Fence, strings.NewReader("stale")); err == nil {
			t.Fatal("stale projection accepted")
		}
		if successor.Epoch() == current.Epoch() {
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
		if _, err := successor.Control(t.Context(), req); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("old epoch command: %v", err)
		}
	})
}
