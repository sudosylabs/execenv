package isolated

import (
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// The stub helper's simulated kernel feeds the real helper pipeline, the
// guest link and the host's retention.
func TestProcessConformance(t *testing.T) {
	execenvtest.RunProcesses(t, func(t *testing.T) execenvtest.Traced {
		t.Helper()
		launch := &recordingLauncher{}
		host := testHost(t, func() error { return nil }, launch)
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "traced", Image: "default"})
		if err != nil {
			t.Fatal(err)
		}
		launch.mu.Lock()
		workload := launch.instances[0].processes
		launch.mu.Unlock()
		return execenvtest.Traced{Env: env, Fence: runningFence(t, env), Workload: workload}
	})
}
