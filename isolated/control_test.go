package isolated

import (
	"errors"
	"github.com/sudosylabs/execenv"
	"testing"
)

func TestContainmentRequiresPauseAndRecoveryAcknowledgements(t *testing.T) {
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "controlled", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	e := env.(*environment)
	inst := launch.instances[0]
	req := execenv.ControlRequest{Fence: execenv.ProjectionFence{ExecutionGrantID: e.ID(), EnvironmentEpoch: e.Epoch(), FenceRevision: 1}, State: execenv.ControlFrozen}
	inst.mu.Lock()
	inst.pauseErr = execenv.ErrUnavailable
	inst.mu.Unlock()
	receipt, err := e.Control(t.Context(), req)
	if !errors.Is(err, execenv.ErrUnavailable) || receipt.Confirmed {
		t.Fatalf("failed pause acknowledged: %+v %v", receipt, err)
	}
	if _, err := e.Attach(t.Context(), execenv.Window{}); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("failed containment opened gate: %v", err)
	}
	inst.mu.Lock()
	inst.pauseErr = nil
	inst.mu.Unlock()
	receipt, err = e.Control(t.Context(), req)
	if err != nil || !receipt.Confirmed {
		t.Fatalf("retry: %+v %v", receipt, err)
	}
	req.Fence.FenceRevision++
	req.State = execenv.ControlRunning
	inst.mu.Lock()
	inst.resumeErr = execenv.ErrUnavailable
	inst.mu.Unlock()
	receipt, err = e.Control(t.Context(), req)
	if err == nil || receipt.Confirmed {
		t.Fatalf("failed resume acknowledged: %+v %v", receipt, err)
	}
	inst.mu.Lock()
	inst.resumeErr = nil
	inst.mu.Unlock()
	_ = e.client.Close()
	receipt, err = e.Control(t.Context(), req)
	inst.mu.Lock()
	paused := inst.paused
	inst.mu.Unlock()
	if err == nil || receipt.Confirmed || !paused {
		t.Fatalf("failed agent recovery escaped containment: %+v %v paused=%v", receipt, err, paused)
	}
}

func TestFailedRevokeRetainsCapacityAndCanRetry(t *testing.T) {
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "controlled", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	inst := launch.instances[0]
	inst.mu.Lock()
	inst.stopErr = execenv.ErrUnavailable
	inst.mu.Unlock()
	if err := env.Revoke(t.Context()); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("failed stop acknowledged: %v", err)
	}
	host.mu.Lock()
	retained := host.grants[env.ID()] != nil
	host.mu.Unlock()
	if !retained {
		t.Fatal("failed stop released identity")
	}
	inst.mu.Lock()
	inst.stopErr = nil
	inst.mu.Unlock()
	if err := env.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	retained = host.grants[env.ID()] != nil
	host.mu.Unlock()
	if retained {
		t.Fatal("successful retry retained identity")
	}
}

func TestRequiredJournalRefusesUncontainedGuestHelper(t *testing.T) {
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	host.cfg.RequireJournal = true
	if !host.Capabilities().JournalProjection || !host.Capabilities().SemanticObservations {
		t.Fatal("required contract not discoverable")
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "journal-required", Image: "default"}); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("uncontained helper accepted as journal capable: %v", err)
	}
}
