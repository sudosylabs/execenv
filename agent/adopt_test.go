package agent_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/internal/procs"
)

// link serves one host link on a and returns the host's client for it.
func link(t *testing.T, a *agent.Agent) (*agent.Client, net.Conn) {
	t.Helper()
	host, guest := net.Pipe()
	go func() { _ = a.Serve(t.Context(), guest) }()
	cli := agent.NewClient(host)
	if err := cli.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	return cli, host
}

// A host that restarts dials again and finds the shell it left: still
// running, with its state, its output at the same offsets and its input
// offset, and the process log it had not acknowledged.
func TestRestartedHostAdoptsTheGuestsProcesses(t *testing.T) {
	source := newFakeProcesses()
	a, err := agent.New(agent.Config{Home: t.TempDir(), Barrier: quiescent{}, Processes: source})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)

	first, firstConn := link(t, a)
	m := procs.New(first.Start)
	sh := shellOn(t, m, "sh")
	if err := sh.typeLine("kept=across; echo one\"\"1"); err != nil {
		t.Fatal(err)
	}
	sh.expect("one1")
	before, err := m.Process("sh")
	if err != nil {
		t.Fatal(err)
	}
	source.events <- execenv.ProcessEvent{Kind: execenv.ProcessStarted, PID: 9, ParentPID: 1, Argv: []string{"x"}, At: time.Now()}
	// The host goes away without a word.
	_ = firstConn.Close()
	m.End(execenv.ErrConnection)

	second, _ := link(t, a)
	held, err := second.Processes(t.Context())
	if err != nil || len(held) != 1 {
		t.Fatalf("Processes() = %+v, %v", held, err)
	}
	gp := held[0]
	if gp.Spec.Tag != "sh" || !gp.Info.Running || gp.Info.PID != before.PID || gp.Info.Input != before.Input || gp.Info.Output[execenv.StreamPTY] < before.Output[execenv.StreamPTY] {
		t.Fatalf("held = %+v, before = %+v", gp, before)
	}
	adopted := procs.New(second.Start)
	t.Cleanup(func() { adopted.End(execenv.ErrClosed) })
	backend, err := second.Adopt(gp)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopted.Adopt(procs.Adopted{Spec: gp.Spec, Info: gp.Info, From: gp.First}, backend); err != nil {
		t.Fatal(err)
	}
	att, err := adopted.Attach("sh", map[execenv.Stream]uint64{execenv.StreamPTY: before.Output[execenv.StreamPTY]})
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	if _, err := adopted.WriteInput("sh", gp.Info.Input, []byte("echo $kept-x\n")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var seen strings.Builder
	for !strings.Contains(seen.String(), "across-x") {
		ev, err := att.Next(ctx)
		if err != nil {
			t.Fatalf("adopted Next() = %v (saw %q)", err, seen.String())
		}
		if ev.Kind == execenv.OutputData {
			if ev.Offset != before.Output[execenv.StreamPTY]+uint64(seen.Len()) {
				t.Fatalf("adopted output at %d, want %d", ev.Offset, before.Output[execenv.StreamPTY]+uint64(seen.Len()))
			}
			seen.Write(ev.Data)
		}
	}
	page, err := second.ObserveProcesses(t.Context(), 0, 10)
	if err != nil || len(page.Events) != 1 || page.Events[0].PID != 9 {
		t.Fatalf("process log after the restart = %+v, %v", page, err)
	}
}

func TestAdoptionAppliesTimeoutThatExpiredWhileDisconnected(t *testing.T) {
	a, err := agent.New(agent.Config{Home: t.TempDir(), Barrier: quiescent{}, Processes: newFakeProcesses()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	first, conn := link(t, a)
	spec := execenv.ProcessSpec{Tag: "overdue", Argv: []string{"sleep", "30"}, Timeout: time.Minute}
	if _, err := first.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	second, newConn := link(t, a)
	t.Cleanup(func() { _ = newConn.Close() })
	held, err := second.Processes(t.Context())
	if err != nil || len(held) != 1 || !held[0].Info.Running {
		t.Fatalf("Processes() after reconnect = %+v, %v", held, err)
	}
	gp := held[0]
	backend, err := second.Adopt(gp)
	if err != nil {
		t.Fatal(err)
	}
	clock := &deadlineClock{base: gp.Info.Started}
	clock.advance(spec.Timeout)
	m := procs.NewWithClock(second.Start, clock.now)
	t.Cleanup(func() { m.End(execenv.ErrClosed) })
	if err := m.Adopt(procs.Adopted{Spec: gp.Spec, Info: gp.Info, From: gp.First}, backend); err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(spec.Tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	expectDeadlineExit(t, att, execenv.ExitStatus{Signal: int(syscall.SIGTERM)})
}

// A link cut during an attempt does not confirm process exit. Keep the
// old backend's synthetic exit separate from the retained process state,
// as the isolated host does during suspension and restoration.
type interruptedKill struct {
	procs.Backend
	disconnect func() error
	wait       <-chan struct{}
	result     chan error
}

func (b interruptedKill) Wait() (execenv.ExitStatus, time.Time) {
	<-b.wait
	return b.Backend.Wait()
}

func (b interruptedKill) Signal(signal execenv.Signal) error {
	if signal != execenv.SignalKill {
		return b.Backend.Signal(signal)
	}
	_ = b.disconnect()
	err := b.Backend.Signal(signal)
	b.result <- err
	return err
}

func TestDeadlineRetriesInterruptedKillOnNewLink(t *testing.T) {
	home := t.TempDir()
	a, err := agent.New(agent.Config{Home: home, Barrier: quiescent{}, Processes: newFakeProcesses()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	first, _ := link(t, a)
	wait := make(chan struct{})
	defer close(wait)
	result := make(chan error, 1)
	clock := &deadlineClock{base: time.Now()}
	m := procs.NewWithClock(func(ctx context.Context, spec execenv.ProcessSpec) (procs.Backend, error) {
		b, err := first.Start(ctx, spec)
		return interruptedKill{Backend: b, disconnect: first.Close, wait: wait, result: result}, err
	}, clock.now)
	spec := execenv.ProcessSpec{
		Tag: "interrupted-kill", Timeout: time.Minute, PTY: &execenv.Window{},
		Argv: []string{"sh", "-c", "trap 'echo term-trapped' TERM; echo ready > ready; while :; do sleep 1; done"},
	}
	if _, err := m.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(spec.Tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	waitFile(t, filepath.Join(home, "ready"))
	clock.advance(spec.Timeout)
	m.Thaw()
	expectDeadlineOutput(t, att, "term-trapped")
	clock.advance(5 * time.Second)
	m.Thaw()
	select {
	case err := <-result:
		if !errors.Is(err, execenv.ErrClosed) {
			t.Fatalf("KILL after link close = %v, want ErrClosed", err)
		}
	default:
		t.Fatal("KILL was not attempted")
	}
	m.Freeze()
	saved := m.SuspendDeadlines()[spec.Tag]

	second, conn := link(t, a)
	defer conn.Close()
	held, err := second.Processes(t.Context())
	if err != nil || len(held) != 1 || !held[0].Info.Running {
		t.Fatalf("held after reconnect = %v, %v", held, err)
	}
	backend, err := second.Adopt(held[0])
	if err != nil {
		t.Fatal(err)
	}
	restored := procs.NewWithClock(second.Start, clock.now)
	restored.Freeze()
	if err := restored.Adopt(procs.Adopted{Spec: held[0].Spec, Info: held[0].Info, From: held[0].First, Deadlines: &saved}, backend); err != nil {
		t.Fatal(err)
	}
	restored.Thaw()
	after, err := restored.Attach(spec.Tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	expectDeadlineExit(t, after, execenv.ExitStatus{Signal: int(syscall.SIGKILL)})
}

func TestAdoptionRetainsProcessExitTime(t *testing.T) {
	a, err := agent.New(agent.Config{Home: t.TempDir(), Barrier: quiescent{}, Processes: newFakeProcesses()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	first, oldConn := link(t, a)
	m := procs.New(first.Start)
	spec := execenv.ProcessSpec{Tag: "finished", Argv: []string{"sh", "-c", "exit 7"}}
	if _, err := m.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(spec.Tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	expectDeadlineExit(t, att, execenv.ExitStatus{Code: 7})
	before, err := m.Process(spec.Tag)
	if err != nil || before.Ended.IsZero() || before.Ended.Before(before.Started) {
		t.Fatalf("Process before adoption = %+v, %v", before, err)
	}
	if err := oldConn.Close(); err != nil {
		t.Fatal(err)
	}

	second, conn := link(t, a)
	defer conn.Close()
	held, err := second.Processes(t.Context())
	if err != nil || len(held) != 1 || held[0].Info.Running || !held[0].Info.Ended.Equal(before.Ended) {
		t.Fatalf("guest process after reconnect = %+v, %v", held, err)
	}
	gp := held[0]
	backend, err := second.Adopt(gp)
	if err != nil {
		t.Fatal(err)
	}
	restored := procs.New(second.Start)
	if err := restored.Adopt(procs.Adopted{Spec: gp.Spec, Info: gp.Info, From: gp.First}, backend); err != nil {
		t.Fatal(err)
	}
	after, err := restored.Process(spec.Tag)
	if err != nil || after.Running || !after.Ended.Equal(before.Ended) || !after.Started.Equal(before.Started) || after.Exit != before.Exit {
		t.Fatalf("adopted Process = %+v, %v; before %+v", after, err, before)
	}
}
