package agent_test

import (
	"context"
	"net"
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
	spec := execenv.ProcessSpec{Tag: "overdue", Argv: []string{"sleep", "30"}, Timeout: 250 * time.Millisecond}
	if _, err := first.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(spec.Timeout + 100*time.Millisecond)

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
	m := procs.New(second.Start)
	t.Cleanup(func() { m.End(execenv.ErrClosed) })
	if err := m.Adopt(procs.Adopted{Spec: gp.Spec, Info: gp.Info, From: gp.First}, backend); err != nil {
		t.Fatal(err)
	}
	att, err := m.Attach(spec.Tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	event, err := att.Next(ctx)
	if err != nil || event.Kind != execenv.OutputExit || event.Exit.Signal != int(syscall.SIGTERM) {
		t.Fatalf("adopted process event = %+v, %v", event, err)
	}
}
