package isolated

import (
	"context"
	"errors"
	"testing"

	"github.com/sudosylabs/execenv"
)

type launchWithFailedCleanup struct{ recordingLauncher }

func (l *launchWithFailedCleanup) Start(ctx context.Context, req startRequest) (instance, error) {
	inst, err := l.recordingLauncher.Start(ctx, req)
	if err != nil {
		return inst, err
	}
	if l.starts == 1 {
		s := inst.(*stubInstance)
		s.mu.Lock()
		s.stopErr = execenv.ErrUnavailable
		s.mu.Unlock()
		return inst, execenv.ErrUnavailable
	}
	return inst, nil
}

func TestFailedLaunchRetainsResourcesUntilRevoke(t *testing.T) {
	launch := &launchWithFailedCleanup{}
	h := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "retry", Image: "default"}
	if _, err := h.Ensure(t.Context(), spec); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("failed start: %v", err)
	}
	if _, err := h.Ensure(t.Context(), spec); !errors.Is(err, execenv.ErrRevoked) {
		t.Fatalf("same ID before cleanup: %v", err)
	}
	report, err := h.Ready(t.Context())
	if err != nil || report.MemoryBytes.Reserved == 0 {
		t.Fatalf("failed launch reservation: %+v, %v", report.MemoryBytes, err)
	}
	first := launch.instance(spec.ID)
	first.mu.Lock()
	first.stopErr = nil
	first.mu.Unlock()
	if err := h.Revoke(t.Context(), spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Ensure(t.Context(), spec); err != nil {
		t.Fatalf("Ensure after cleanup: %v", err)
	}
}

func TestSuspendStopFailureCanBeCleanedUpByRevoke(t *testing.T) {
	launch := &recordingLauncher{}
	h := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "suspend-stop", Image: "default"}
	held, err := h.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	env := h.grants[spec.ID]
	first := launch.instance(spec.ID)
	first.mu.Lock()
	first.stopErr = execenv.ErrUnavailable
	first.mu.Unlock()
	if _, err := env.control.Idle(t.Context(), func(done execenv.ControlReceipt) error {
		return env.suspend(t.Context(), done)
	}); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("suspend: %v", err)
	}
	first.mu.Lock()
	first.stopErr = nil
	first.mu.Unlock()
	if err := held.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if !first.stopped {
		t.Fatal("revoke did not stop the suspended grant's original instance")
	}
}
