package memory_test

import (
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

func TestShellEchoesInput(t *testing.T) {
	t.Parallel()
	host, err := memory.New(memory.Config{
		Images:   []execenv.Image{"default"},
		Capacity: execenv.DefaultGrant.Times(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	sh := execenvtest.StartShell(t, env, fence, "sh")
	if err := sh.Type("ping"); err != nil {
		t.Fatal(err)
	}
	sh.Expect("ping")
}

func TestConformance(t *testing.T) {
	execenvtest.Run(t, func(t *testing.T, cfg execenvtest.Config) execenv.Host {
		t.Helper()
		host, err := memory.New(memory.Config{
			Images:   []execenv.Image{"default", "alt"},
			Capacity: execenv.DefaultGrant.Times(2),
		})
		if err != nil {
			t.Fatal(err)
		}
		return host
	})
}

func TestLossConformance(t *testing.T) {
	execenvtest.RunLoss(t, func(t *testing.T) execenvtest.Lossy {
		t.Helper()
		host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(2)})
		if err != nil {
			t.Fatal(err)
		}
		return execenvtest.Lossy{Host: host, Lose: func(t *testing.T, id execenv.ID) {
			if !host.Lose(id) {
				t.Fatalf("Lose(%s) found no live grant", id)
			}
		}}
	})
}
