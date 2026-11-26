package isolated

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// A machine whose monitor dies is a lost grant through the public API.
func TestLossConformance(t *testing.T) {
	execenvtest.RunLoss(t, func(t *testing.T) execenvtest.Lossy {
		t.Helper()
		launch := &recordingLauncher{}
		host := testHost(t, func() error { return nil }, launch)
		return execenvtest.Lossy{Host: host, Lose: func(t *testing.T, id execenv.ID) {
			inst := launch.instance(id)
			if inst == nil {
				t.Fatalf("no machine for %s", id)
			}
			inst.die()
		}}
	})
}

func waitLost(t *testing.T, env execenv.Env) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := env.ControlStatus(t.Context()); errors.Is(err, execenv.ErrLost) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the host did not notice the loss")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A guest link that breaks while the machine runs is a loss too: the
// machine is stopped and its grant directory removed, like a revoke.
func TestLostGuestLinkStopsMachineAndCleansUp(t *testing.T) {
	t.Parallel()
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	inst := launch.instance("grant-1")
	grantDir := filepath.Join(host.cfg.WorkDir, "grants", "grant-1")
	if _, err := os.Stat(grantDir); err != nil {
		t.Fatal(err)
	}
	inst.dropLink()
	waitLost(t, env)
	inst.mu.Lock()
	stopped := inst.stopped
	inst.mu.Unlock()
	if !stopped {
		t.Fatal("lost machine was not stopped")
	}
	// Ready waits for the host to finish retiring the grant.
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Only the lost record stays, so a restarted host keeps the id lost.
	entries, err := os.ReadDir(grantDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != stateName {
		t.Fatalf("grant directory after loss: %v %v", entries, err)
	}
	if report.MemoryBytes.Reserved != 0 || report.Grants != 0 || report.Lost != 1 {
		t.Fatalf("Ready() after loss = %+v", report)
	}
}

// A revoke is not a loss: the watcher must not count it or keep the id.
func TestRevokeIsNotCountedAsLoss(t *testing.T) {
	t.Parallel()
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Lost != 0 || report.Grants != 0 {
		t.Fatalf("Ready() after revoke = %+v", report)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
		t.Fatalf("Ensure() after revoke error = %v", err)
	}
}

// A failed release after a loss keeps the slot held until Revoke retries it.
func TestFailedLossCleanupKeepsSlotUntilRevoke(t *testing.T) {
	t.Parallel()
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	inst := launch.instance("grant-1")
	inst.mu.Lock()
	inst.stopErr = errors.New("release failed")
	inst.mu.Unlock()
	inst.die()
	waitLost(t, env)
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.MemoryBytes.Reserved != execenv.DefaultGrant.MemoryBytes || report.Grants != 0 || report.Lost != 1 {
		t.Fatalf("Ready() with unreleased loss = %+v", report)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); !errors.Is(err, execenv.ErrLost) {
		t.Fatalf("Ensure() error = %v, want ErrLost", err)
	}
	inst.mu.Lock()
	inst.stopErr = nil
	inst.mu.Unlock()
	if err := host.Revoke(t.Context(), "grant-1"); err != nil {
		t.Fatal(err)
	}
	if report, err = host.Ready(t.Context()); err != nil || report.MemoryBytes.Reserved != 0 {
		t.Fatalf("Ready() after revoke = %+v %v", report, err)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
		t.Fatalf("Ensure() after revoke error = %v", err)
	}
}

func TestDrainingRefusesNewGrantsAndServesHeldOnes(t *testing.T) {
	t.Parallel()
	host := testHost(t, func() error { return nil }, &recordingLauncher{})
	held, err := host.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	host.SetDraining(true)
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usable || report.Reason != execenv.ReasonDraining || report.Grants != 1 || report.MemoryBytes.Reserved != execenv.DefaultGrant.MemoryBytes {
		t.Fatalf("Ready() while draining = %+v", report)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "new", Image: "default"}); !errors.Is(err, execenv.ErrDraining) {
		t.Fatalf("Ensure() while draining error = %v, want ErrDraining", err)
	}
	again, err := host.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"})
	if err != nil || again != held {
		t.Fatalf("Ensure() of a held grant while draining: %v", err)
	}
	if _, err := held.ControlStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	host.SetDraining(false)
	if report, err = host.Ready(t.Context()); err != nil || !report.Usable || report.Reason != "" {
		t.Fatalf("Ready() after undrain = %+v %v", report, err)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "new", Image: "default"}); err != nil {
		t.Fatal(err)
	}
}

func TestLossReceiptSurvivesFailedStopAndRestart(t *testing.T) {
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "kept", Image: "default"}
	env, err := host.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	inst := launch.instance(spec.ID)
	inst.mu.Lock()
	inst.stopErr = errors.New("stop did not complete")
	inst.mu.Unlock()
	inst.dropLink()
	waitLost(t, env)
	inst.mu.Lock()
	inst.stopErr = nil
	inst.mu.Unlock()

	next := restart(t, host)
	if _, err := next.Ensure(t.Context(), spec); !errors.Is(err, execenv.ErrLost) {
		t.Fatalf("Ensure after loss and restart: %v, want ErrLost", err)
	}
	select {
	case <-inst.Done():
	default:
		t.Fatal("recovery kept the machine whose stop failed")
	}
}
