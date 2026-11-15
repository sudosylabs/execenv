package procs

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// pipeProc is a backend whose output the test writes.
type pipeProc struct {
	out  *io.PipeReader
	w    *io.PipeWriter
	in   bytes.Buffer
	mu   sync.Mutex
	done chan struct{}
	once sync.Once
}

func newPipeProc() *pipeProc {
	r, w := io.Pipe()
	return &pipeProc{out: r, w: w, done: make(chan struct{})}
}

func (p *pipeProc) Streams() map[execenv.Stream]io.Reader {
	return map[execenv.Stream]io.Reader{execenv.StreamPTY: p.out}
}
func (p *pipeProc) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.in.Write(b)
}
func (p *pipeProc) CloseInput() error                            { return nil }
func (p *pipeProc) Resize(context.Context, execenv.Window) error { return nil }
func (p *pipeProc) Signal(execenv.Signal) error                  { p.exit(); return nil }
func (p *pipeProc) Wait() execenv.ExitStatus                     { <-p.done; return execenv.ExitStatus{Signal: 9} }
func (p *pipeProc) PID() int                                     { return 1 }
func (p *pipeProc) Screen(context.Context) (execenv.Screen, error) {
	return execenv.Screen{Offset: 1}, nil
}
func (p *pipeProc) exit() {
	p.once.Do(func() { _ = p.w.Close(); close(p.done) })
}

func start(t *testing.T, history int64) (*Manager, *pipeProc) {
	t.Helper()
	p := newPipeProc()
	m := New(func(context.Context, execenv.ProcessSpec) (Backend, error) { return p, nil })
	if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "p", PTY: &execenv.Window{}, HistoryBytes: history}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.exit)
	return m, p
}

func waitOutput(t *testing.T, m *Manager, total uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, _ := m.Process("p"); info.Output[execenv.StreamPTY] >= total {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("output never reached %d", total)
}

// A reader behind the history is told what it lost, then reads on from
// the oldest byte held, by absolute offset.
func TestHistoryTruncatesByAbsoluteOffset(t *testing.T) {
	m, p := start(t, 8)
	if _, err := p.w.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, m, 16)
	a, err := m.Attach("p", map[execenv.Stream]uint64{execenv.StreamPTY: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ev, err := a.Next(t.Context())
	if err != nil || ev.Kind != execenv.OutputTruncated || ev.Offset != 2 || ev.To != 8 {
		t.Fatalf("first event = %+v, %v; want truncation 2..8", ev, err)
	}
	var got strings.Builder
	for got.Len() < 8 {
		ev, err := a.Next(t.Context())
		if err != nil || ev.Kind != execenv.OutputData || ev.Offset != 8+uint64(got.Len()) {
			t.Fatalf("data = %+v, %v", ev, err)
		}
		got.Write(ev.Data)
	}
	if got.String() != "89abcdef" {
		t.Fatalf("held = %q", got.String())
	}
	p.exit()
	if ev, err := a.Next(t.Context()); err != nil || ev.Kind != execenv.OutputExit || ev.Exit.Signal != 9 {
		t.Fatalf("exit = %+v, %v", ev, err)
	}
	if _, err := a.Next(t.Context()); err != io.EOF {
		t.Fatalf("after exit = %v, want io.EOF", err)
	}
}

// Input overlapping what was taken is dropped; a gap is a conflict.
func TestInputIsTakenOnceByOffset(t *testing.T) {
	m, p := start(t, 0)
	if next, err := m.WriteInput("p", 0, []byte("abc")); err != nil || next != 3 {
		t.Fatalf("first = %d, %v", next, err)
	}
	if next, err := m.WriteInput("p", 1, []byte("bcde")); err != nil || next != 5 {
		t.Fatalf("overlap = %d, %v", next, err)
	}
	if _, err := m.WriteInput("p", 9, []byte("x")); err != execenv.ErrConflict {
		t.Fatalf("gap = %v, want ErrConflict", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.in.String() != "abcde" {
		t.Fatalf("taken = %q", p.in.String())
	}
}

// Ending the manager fails readers with the cause, not an exit.
func TestEndFailsReadersWithTheCause(t *testing.T) {
	m, _ := start(t, 0)
	a, err := m.Attach("p", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.End(execenv.ErrLost)
	if _, err := a.Next(t.Context()); err != execenv.ErrLost {
		t.Fatalf("Next after End = %v, want ErrLost", err)
	}
	if _, err := m.Process("p"); err != execenv.ErrLost {
		t.Fatalf("Process after End = %v, want ErrLost", err)
	}
}
