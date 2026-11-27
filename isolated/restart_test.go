package isolated

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/internal/capacity"
)

// crash ends h as a killed daemon would: its links to the guests drop and
// its lock goes, but nothing is revoked and no machine is stopped.
func (h *Host) crash() {
	h.mu.Lock()
	envs := make([]*environment, 0, len(h.grants))
	for _, env := range h.grants {
		envs = append(envs, env)
	}
	h.grants = map[execenv.ID]*environment{}
	h.mu.Unlock()
	for _, env := range envs {
		env.goneOnce.Do(func() { close(env.gone) })
		env.timer.Stop()
		env.mu.Lock()
		client := env.client
		env.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
	}
	_ = h.lock.Close()
}

// restart serves h's work directory again with the same launcher.
func restart(t *testing.T, h *Host) *Host {
	t.Helper()
	h.crash()
	pool, err := capacity.New(h.cfg.Capacity, h.cfg.Defaults, h.cfg.GrantMax)
	if err != nil {
		t.Fatal(err)
	}
	next, err := newHost(h.cfg, pool, h.images, h.probe, h.launch, h.attach)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range []execenv.ID{"kept", "paused", "gone"} {
			_ = next.Revoke(context.Background(), id)
		}
		_ = next.lock.Close()
	})
	return next
}

// A restarted host adopts a running grant: same epoch and fence, the shell
// still running with its state, its output at the same offsets.
func TestRestartAdoptsARunningGrant(t *testing.T) {
	launch := &recordingLauncher{}
	h := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "kept", Image: "default"}
	env, err := h.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	sh := execenvtest.StartShell(t, env, fence, "sh")
	if err := sh.Type("kept=across; echo one\"\"1"); err != nil {
		t.Fatal(err)
	}
	sh.Expect("one1")
	mark := sh.Offset()
	before, err := env.Process(t.Context(), "sh")
	if err != nil {
		t.Fatal(err)
	}

	h2 := restart(t, h)
	again, err := h2.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if again.Epoch() != env.Epoch() {
		t.Fatalf("epoch %q after restart, want %q", again.Epoch(), env.Epoch())
	}
	receipt, err := again.ControlStatus(t.Context())
	if err != nil || receipt.Fence != fence || receipt.State != execenv.ControlRunning {
		t.Fatalf("ControlStatus() = %+v, %v", receipt, err)
	}
	info, err := again.Process(t.Context(), "sh")
	if err != nil || !info.Running || info.PID != before.PID || info.Input != before.Input {
		t.Fatalf("Process() = %+v, %v; before %+v", info, err, before)
	}
	a, err := again.AttachProcess(t.Context(), "sh", map[execenv.Stream]uint64{execenv.StreamPTY: mark})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := again.WriteInput(t.Context(), fence, "sh", info.Input, []byte("echo $kept-x\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var seen strings.Builder
	for !strings.Contains(seen.String(), "across-x") {
		ev, err := a.Next(ctx)
		if err != nil {
			t.Fatalf("Next() = %v (saw %q)", err, seen.String())
		}
		if ev.Kind == execenv.OutputData {
			if ev.Offset != mark+uint64(seen.Len()) {
				t.Fatalf("resumed at %d, want %d", ev.Offset, mark+uint64(seen.Len()))
			}
			seen.Write(ev.Data)
		}
	}
	if launch.starts != 1 {
		t.Fatalf("restart booted %d machines, want the one", launch.starts)
	}
}

// A grant frozen when its host went away stays frozen, and thaws under its
// next fence with its processes.
func TestRestartAdoptsAFrozenGrant(t *testing.T) {
	launch := &recordingLauncher{}
	h := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "paused", Image: "default"}
	env, err := h.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	sh := execenvtest.StartShell(t, env, fence, "sh")
	// Wait for the shell to have run it: one that has not started reading
	// yet may discard what was typed ahead.
	if err := sh.Type(`kept=frozen; echo set""-done`); err != nil {
		t.Fatal(err)
	}
	sh.Expect("set-done")
	frozen := fence
	frozen.FenceRevision++
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: frozen, State: execenv.ControlFrozen}); err != nil {
		t.Fatal(err)
	}

	h2 := restart(t, h)
	again, err := h2.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := again.ControlStatus(t.Context()); err != nil || receipt.State != execenv.ControlFrozen || receipt.Fence != frozen {
		t.Fatalf("ControlStatus() = %+v, %v", receipt, err)
	}
	if _, err := again.WriteInput(t.Context(), frozen, "sh", 0, []byte("x")); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("WriteInput() while frozen = %v, want ErrFrozen", err)
	}
	thawed := frozen
	thawed.FenceRevision++
	if _, err := again.Control(t.Context(), execenv.ControlRequest{Fence: thawed, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	info, err := again.Process(t.Context(), "sh")
	if err != nil || !info.Running {
		t.Fatalf("Process() after thaw = %+v, %v", info, err)
	}
	typeAndExpect(t, again, thawed, "sh", info, "echo $kept-y", "frozen-y")
}

// typeAndExpect types line into a running process at its input offset and
// reads its output, from where it stood, until marker appears.
func typeAndExpect(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, tag string, info execenv.ProcessInfo, line, marker string) {
	t.Helper()
	a, err := env.AttachProcess(t.Context(), tag, info.Output)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := env.WriteInput(t.Context(), fence, tag, info.Input, []byte(line+"\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var seen strings.Builder
	for !strings.Contains(seen.String(), marker) {
		ev, err := a.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (saw %q)", marker, err, seen.String())
		}
		seen.Write(ev.Data)
	}
}

// A grant whose machine died while no host ran is lost after the restart,
// and stays lost until revoked.
func TestRestartFindsADeadMachineLost(t *testing.T) {
	launch := &recordingLauncher{}
	h := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "gone", Image: "default"}
	env, err := h.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	h.crash()
	launch.instance("gone").die()
	pool, _ := capacity.New(h.cfg.Capacity, h.cfg.Defaults, h.cfg.GrantMax)
	h2, err := newHost(h.cfg, pool, h.images, h.probe, h.launch, h.attach)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h2.lock.Close() })
	if _, err := h2.Ensure(t.Context(), spec); !errors.Is(err, execenv.ErrLost) {
		t.Fatalf("Ensure() of a grant lost while down = %v, want ErrLost", err)
	}
	if err := h2.Revoke(t.Context(), "gone"); err != nil {
		t.Fatal(err)
	}
	fresh, err := h2.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Epoch() == env.Epoch() {
		t.Fatal("a revoked loss came back with its old epoch")
	}
	_ = h2.Revoke(context.Background(), "gone")
}

func TestRestartKeepsTheOriginalBootArtifacts(t *testing.T) {
	launch := &recordingLauncher{}
	h := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "kept", Image: "default"}
	if _, err := h.Ensure(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	original := h.grants[spec.ID].image
	next := writeCatalogImage(t, t.TempDir(), "default", "updated")
	h.images[spec.Image] = &imageRecord{image: next}
	recovered := restart(t, h)
	if got := recovered.grants[spec.ID].image; got != original {
		t.Fatalf("recovery attached different boot artifacts to the live grant: %v", got.ID)
	}
}
