package remote_test

import (
	"errors"
	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
	"testing"
)

func TestControlHighWaterSurvivesIndependentConnections(t *testing.T) {
	inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}})
	if err != nil {
		t.Fatal(err)
	}
	first, second := serveInner(t, inner), serveInner(t, inner)
	spec := execenv.Spec{ID: "shared", Image: "default"}
	a, err := first.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	one, two := a.(execenv.ControlledEnv), b.(execenv.ControlledEnv)
	if one.Epoch() != two.Epoch() {
		t.Fatal("connection invented epoch")
	}
	request := execenv.ControlRequest{Fence: execenv.ProjectionFence{ExecutionGrantID: spec.ID, EnvironmentEpoch: one.Epoch(), FenceRevision: 100}, State: execenv.ControlFrozen}
	if _, err := one.Control(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.State = execenv.ControlRunning
	request.Fence.FenceRevision = 99
	if _, err := two.Control(t.Context(), request); !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("other connection reopened stale fence: %v", err)
	}
	receipt, err := two.ControlStatus(t.Context())
	if err != nil || receipt.Fence.FenceRevision != 100 || receipt.State != execenv.ControlFrozen || !receipt.Confirmed {
		t.Fatalf("status %+v %v", receipt, err)
	}
	if first.Capabilities().Isolated || !first.Capabilities().OrderedControl {
		t.Fatalf("remote invented adapter capabilities: %+v", first.Capabilities())
	}
}
