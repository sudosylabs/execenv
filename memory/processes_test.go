package memory_test

import (
	"slices"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

func tracedMemory(t *testing.T) (execenv.Env, execenv.ProjectionFence) {
	t.Helper()
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(1)})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "traced", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	return env, fence
}

func TestProcessConformance(t *testing.T) {
	execenvtest.RunProcesses(t, func(t *testing.T) execenvtest.Traced {
		env, fence := tracedMemory(t)
		return execenvtest.Traced{Env: env, Fence: fence, Workload: execenvtest.TerminalWorkload(t, env)}
	})
}

// The fake shell records itself, every command line, 127 for an unknown
// command, and its own exit.
func TestShellRecordsItsCommandLines(t *testing.T) {
	env, fence := tracedMemory(t)
	term, err := env.Attach(t.Context(), execenv.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out := execenvtest.Watch(t, term)
	if _, err := term.Write([]byte("NAME=x\necho $NAME\nmissing a\nexit\n")); err != nil {
		t.Fatal(err)
	}
	out.WaitFor("not found")
	var events []execenv.ProcessEvent
	deadline := time.Now().Add(5 * time.Second)
	for len(events) < 6 && time.Now().Before(deadline) {
		page, err := env.ObserveProcesses(t.Context(), fence, uint64(len(events)), execenv.MaxProcessPage)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, page.Events...)
		time.Sleep(time.Millisecond)
	}
	if len(events) != 6 {
		t.Fatalf("events: %+v", events)
	}
	shell := events[0]
	if shell.Kind != execenv.ProcessStarted || !slices.Equal(shell.Argv, []string{"/bin/sh", "-l"}) || shell.Dir != execenv.GuestWorkspace {
		t.Fatalf("shell: %+v", shell)
	}
	echo, missing := events[1], events[3]
	if !slices.Equal(echo.Argv, []string{"echo", "x"}) || echo.ParentPID != shell.PID || *events[2].ExitCode != 0 {
		t.Fatalf("echo: %+v %+v", echo, events[2])
	}
	if !slices.Equal(missing.Argv, []string{"missing", "a"}) || events[4].PID != missing.PID || *events[4].ExitCode != 127 {
		t.Fatalf("unknown command: %+v %+v", missing, events[4])
	}
	if events[5].PID != shell.PID || *events[5].ExitCode != 0 {
		t.Fatalf("shell exit: %+v", events[5])
	}
}
