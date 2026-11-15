package remote_test

import (
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

// The remote wire carries the process log of the served host.
func TestProcessConformance(t *testing.T) {
	execenvtest.RunProcesses(t, func(t *testing.T) execenvtest.Traced {
		t.Helper()
		inner, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(1)})
		if err != nil {
			t.Fatal(err)
		}
		env, err := serveInner(t, inner).Ensure(t.Context(), execenv.Spec{ID: "traced", Image: "default"})
		if err != nil {
			t.Fatal(err)
		}
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		return execenvtest.Traced{Env: env, Fence: fence, Workload: execenvtest.TerminalWorkload(t, env)}
	})
}
