package memory_test

import (
	"errors"
	"testing"
	"time"

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

// A suspended grant keeps only its disk; while another grant holds its CPU
// and memory, its thaw is ErrCapacity and it stays suspended.
func TestThawOfASuspendedGrantNeedsItsResources(t *testing.T) {
	total := execenv.DefaultGrant
	total.DiskBytes *= 2
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: total})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "parked", Image: "default", IdleTimeout: 50 * time.Millisecond, IdleAction: execenv.IdleSuspend})
	if err != nil {
		t.Fatal(err)
	}
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlFrozen}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		report, _ := host.Ready(t.Context())
		if report.MemoryBytes.Reserved == 0 && report.DiskBytes.Reserved == execenv.DefaultGrant.DiskBytes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not suspended: %+v", report)
		}
		time.Sleep(10 * time.Millisecond)
	}
	other, err := host.Ensure(t.Context(), execenv.Spec{ID: "other", Image: "default"})
	if err != nil {
		t.Fatalf("Ensure() beside a suspended grant: %v", err)
	}
	fence.FenceRevision++
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); !errors.Is(err, execenv.ErrCapacity) {
		t.Fatalf("thaw without room = %v, want ErrCapacity", err)
	}
	if info, err := host.WaitGrant(t.Context(), "parked"); err != nil || info.State != execenv.GrantSuspended {
		t.Fatalf("after a refused thaw: %+v, %v", info, err)
	}
	if err := other.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	fence.FenceRevision++
	if receipt, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil || !receipt.Confirmed {
		t.Fatalf("thaw with room = %+v, %v", receipt, err)
	}
}

func TestPortConformance(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant})
	if err != nil {
		t.Fatal(err)
	}
	execenvtest.RunPorts(t, host, execenvtest.LocalPortListener(host))
}

func TestListeningPortsConformance(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant})
	if err != nil {
		t.Fatal(err)
	}
	execenvtest.RunListeningPorts(t, host, execenvtest.LocalPortListener(host))
}
