//go:build isolation && linux

package isolated

import (
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// TestLiveRestartAdoptsRunningGrant restarts the host under a running
// grant: the machine is not stopped, and the new host adopts it with its
// epoch, its shell and the shell's state.
func TestLiveRestartAdoptsRunningGrant(t *testing.T) {
	dir := t.TempDir()
	host := liveHost(t, 1, dir)
	spec := execenv.Spec{ID: "restart-1", Image: "default"}
	env, err := host.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	sh := execenvtest.StartShell(t, env, fence, "sh")
	if err := sh.Type("kept=across; echo one\"\"1"); err != nil {
		t.Fatal(err)
	}
	sh.Expect("one1")
	host.crash()

	again := liveHost(t, 1, dir)
	t.Cleanup(func() { revokeAll(again) })
	adopted, err := again.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Epoch() != env.Epoch() {
		t.Fatalf("epoch %q after restart, want %q", adopted.Epoch(), env.Epoch())
	}
	info, err := adopted.Process(t.Context(), "sh")
	if err != nil || !info.Running {
		t.Fatalf("Process() after restart = %+v, %v", info, err)
	}
	typeAndExpect(t, adopted, fence, "sh", info, "echo $kept-x", "across-x")
}
