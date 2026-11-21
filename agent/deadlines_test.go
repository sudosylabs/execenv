package agent_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/procs"
)

type deadlineClock struct {
	base   time.Time
	offset atomic.Int64
}

func (c *deadlineClock) now() time.Time          { return c.base.Add(time.Duration(c.offset.Load())) }
func (c *deadlineClock) advance(d time.Duration) { c.offset.Add(int64(d)) }

type recordingSignals struct {
	procs.Backend
	signals chan execenv.Signal
}

func (b recordingSignals) Signal(signal execenv.Signal) error {
	b.signals <- signal
	return b.Backend.Signal(signal)
}

func TestProcessDeadlinesAfterReopen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spec   execenv.ProcessSpec
		signal syscall.Signal
	}{
		{"timeout", execenv.ProcessSpec{Timeout: time.Minute}, syscall.SIGTERM},
		{"detach grace", execenv.ProcessSpec{DetachGrace: time.Minute}, syscall.SIGHUP},
		{"timeout escalates after thaw", execenv.ProcessSpec{Timeout: time.Minute}, syscall.SIGKILL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cli, home := startAgent(t)
			clock := &deadlineClock{base: time.Now()}
			var m *procs.Manager
			m = procs.NewWithClock(func(ctx context.Context, spec execenv.ProcessSpec) (procs.Backend, error) {
				b, err := cli.Start(ctx, spec)
				// Freeze before Start arms the deadlines.
				m.Freeze()
				return b, err
			}, clock.now)
			t.Cleanup(func() { m.End(execenv.ErrClosed) })
			spec := tc.spec
			spec.Tag = "deadline"
			spec.Argv = []string{"sleep", "30"}
			spec.PTY = &execenv.Window{}
			trap := tc.signal == syscall.SIGKILL
			if trap {
				spec.Argv = []string{"sh", "-c", "trap 'echo term-trapped' TERM; echo ready > ready; while :; do sleep 1; done"}
			}
			if _, err := m.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			if trap {
				waitFile(t, filepath.Join(home, "ready"))
			}
			if err := cli.Quiesce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := m.Signal(spec.Tag, execenv.SignalInterrupt); !errors.Is(err, execenv.ErrFrozen) {
				t.Fatalf("caller signal while frozen = %v", err)
			}
			clock.advance(time.Minute)
			if err := cli.Reopen(t.Context()); err != nil {
				t.Fatal(err)
			}
			m.Thaw()
			att, err := m.Attach(spec.Tag, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer att.Close()
			if trap {
				expectDeadlineOutput(t, att, "term-trapped")
				clock.advance(5 * time.Second)
				m.Thaw()
			}
			expectDeadlineExit(t, att, execenv.ExitStatus{Signal: int(tc.signal)})
		})
	}
}

func TestProcessDeadlineSkipsExitBeforeThaw(t *testing.T) {
	cli, home := startAgent(t)
	clock := &deadlineClock{base: time.Now()}
	signals := make(chan execenv.Signal, 4)
	var m *procs.Manager
	m = procs.NewWithClock(func(ctx context.Context, spec execenv.ProcessSpec) (procs.Backend, error) {
		b, err := cli.Start(ctx, spec)
		m.Freeze()
		return recordingSignals{Backend: b, signals: signals}, err
	}, clock.now)
	t.Cleanup(func() { m.End(execenv.ErrClosed) })
	spec := execenv.ProcessSpec{
		Tag: "exit-before-thaw", Timeout: time.Minute,
		Argv: []string{"sh", "-c", "while [ ! -f exit-now ]; do sleep 0.05; done; exit 7"},
	}
	if _, err := m.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(spec.Tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	if err := cli.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	if err := os.WriteFile(filepath.Join(home, "exit-now"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	expectDeadlineExit(t, att, execenv.ExitStatus{Code: 7})
	if err := cli.Reopen(t.Context()); err != nil {
		t.Fatal(err)
	}
	m.Thaw()
	select {
	case signal := <-signals:
		t.Fatalf("exited process was sent %s", signal)
	default:
	}
}

func expectDeadlineOutput(t *testing.T, a execenv.Attachment, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var output strings.Builder
	for !strings.Contains(output.String(), text) {
		event, err := a.Next(ctx)
		if err != nil || event.Kind == execenv.OutputExit {
			t.Fatalf("waiting for %q: got %q, event %+v, %v", text, output.String(), event, err)
		}
		output.Write(event.Data)
	}
}

func expectDeadlineExit(t *testing.T, a execenv.Attachment, want execenv.ExitStatus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		event, err := a.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for exit: %v", err)
		}
		if event.Kind == execenv.OutputExit {
			if event.Exit != want {
				t.Fatalf("exit = %+v, want %+v", event.Exit, want)
			}
			return
		}
	}
}
