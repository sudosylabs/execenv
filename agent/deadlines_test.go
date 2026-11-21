package agent_test

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/procs"
)

func TestProcessDeadlinesAfterReopen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spec   execenv.ProcessSpec
		signal syscall.Signal
		trap   bool
	}{
		{"timeout", execenv.ProcessSpec{Timeout: 250 * time.Millisecond}, syscall.SIGTERM, false},
		{"detach grace", execenv.ProcessSpec{DetachGrace: 250 * time.Millisecond}, syscall.SIGHUP, false},
		{"timeout escalates after thaw", execenv.ProcessSpec{Timeout: 750 * time.Millisecond}, syscall.SIGKILL, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cli, home := startAgent(t)
			m := procs.New(cli.Start)
			t.Cleanup(func() { m.End(execenv.ErrClosed) })
			spec := tc.spec
			spec.Tag = "deadline"
			spec.Argv = []string{"sleep", "30"}
			spec.PTY = &execenv.Window{}
			if tc.trap {
				spec.Argv = []string{"sh", "-c", "trap '' TERM; echo ready > ready; exec sleep 30"}
			}
			if _, err := m.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			if tc.trap {
				waitFile(t, filepath.Join(home, "ready"))
			}
			// Detach grace exercises a deadline refused by the agent
			// before the manager observes a freeze.
			if spec.DetachGrace == 0 {
				m.Freeze()
			}
			if err := cli.Quiesce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := m.Signal(spec.Tag, execenv.SignalInterrupt); !errors.Is(err, execenv.ErrFrozen) {
				t.Fatalf("caller signal while frozen = %v", err)
			}
			time.Sleep(max(spec.Timeout, spec.DetachGrace) + 150*time.Millisecond)
			before, err := m.Process(spec.Tag)
			if err != nil || !before.Running {
				t.Fatalf("process while frozen = %+v, %v", before, err)
			}
			if err := cli.Reopen(t.Context()); err != nil {
				t.Fatal(err)
			}
			thawed := time.Now()
			m.Thaw()
			a, err := m.Attach(spec.Tag, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			budget := 2 * time.Second
			if tc.trap {
				budget = 8 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			for {
				event, err := a.Next(ctx)
				if err != nil {
					t.Fatalf("waiting for exit after reopen: %v", err)
				}
				if event.Kind == execenv.OutputExit {
					if event.Exit.Signal != int(tc.signal) {
						t.Fatalf("exit = %+v, want %s", event.Exit, tc.signal)
					}
					if tc.trap && time.Since(thawed) < 4*time.Second {
						t.Fatal("KILL arrived before the process had time to handle TERM")
					}
					break
				}
			}
		})
	}
}
