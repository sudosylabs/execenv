package execenvtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// floodLines makes a shell print well over execenv.TerminalHistoryBytes.
const floodLines = "250000"

// Output reads one Terminal in the background and keeps what it read.
type Output struct {
	t    *testing.T
	term execenv.Terminal
	mu   sync.Mutex
	cond *sync.Cond
	data []byte
	err  error
	done chan struct{}
}

// Watch starts reading term until Read fails.
func Watch(t *testing.T, term execenv.Terminal) *Output {
	t.Helper()
	o := &Output{t: t, term: term, done: make(chan struct{})}
	o.cond = sync.NewCond(&o.mu)
	go func() {
		defer close(o.done)
		buf := make([]byte, 32<<10)
		for {
			n, err := term.Read(buf)
			o.mu.Lock()
			o.data = append(o.data, buf[:n]...)
			if err != nil {
				o.err = err
			}
			o.cond.Broadcast()
			o.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return o
}

// WaitFor waits until the output contains marker and returns the bytes
// read so far.
func (o *Output) WaitFor(marker string) []byte {
	o.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	timer := time.AfterFunc(time.Until(deadline), func() {
		o.mu.Lock()
		o.cond.Broadcast()
		o.mu.Unlock()
	})
	defer timer.Stop()
	o.mu.Lock()
	defer o.mu.Unlock()
	from := 0
	for {
		if bytes.Contains(o.data[from:], []byte(marker)) {
			return append([]byte(nil), o.data...)
		}
		from = max(0, len(o.data)-len(marker))
		if o.err != nil {
			o.t.Fatalf("terminal ended (%v) before %q", o.err, marker)
		}
		if time.Now().After(deadline) {
			o.t.Fatalf("terminal never printed %q", marker)
		}
		o.cond.Wait()
	}
}

// Stop waits for the reader to end, after Close or a failure, and returns
// everything it read and the error that ended it.
func (o *Output) Stop() ([]byte, error) {
	o.t.Helper()
	select {
	case <-o.done:
	case <-time.After(60 * time.Second):
		o.t.Fatal("terminal reader did not end")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.data...), o.err
}

func send(t *testing.T, term execenv.Terminal, line string) {
	t.Helper()
	if _, err := term.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("Write(%q) error = %v", line, err)
	}
}

func attachTerminal(t *testing.T, env execenv.Env, request execenv.AttachRequest) execenv.Terminal {
	t.Helper()
	term, err := env.Attach(t.Context(), request)
	if err != nil {
		t.Fatalf("Attach(%+v) error = %v", request, err)
	}
	t.Cleanup(func() { _ = term.Close() })
	return term
}

func resumeAt(term execenv.Terminal, offset uint64) execenv.AttachRequest {
	return execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: term.ID(), Offset: offset}}
}

func ensureGrant(t *testing.T, host execenv.Host) execenv.Env {
	t.Helper()
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	t.Cleanup(func() { _ = host.Revoke(context.Background(), "grant-1") })
	return env
}

// Markers are typed with an empty quote pair inside them, so a terminal's
// echo of the command never matches; only the command's output does.

func runTerminal(t *testing.T, factory Factory) {
	t.Run("terminal resume continues exactly", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		if first.Start() != 0 || first.ID() == "" {
			t.Fatalf("new shell Start=%d ID=%q", first.Start(), first.ID())
		}
		out := Watch(t, first)
		send(t, first, `echo one""1`)
		out.WaitFor("one1")
		send(t, first, `echo two""2`)
		out.WaitFor("two2")
		_ = first.Close()
		seen, _ := out.Stop()
		if first.Offset() != first.Start()+uint64(len(seen)) {
			t.Fatalf("Offset = %d after reading %d bytes from %d", first.Offset(), len(seen), first.Start())
		}
		at := bytes.Index(seen, []byte("one1"))
		position := first.Start() + uint64(at)
		second := attachTerminal(t, env, resumeAt(first, position))
		if second.ID() != first.ID() || second.Start() != position {
			t.Fatalf("resume ID=%q Start=%d, want %q at %d", second.ID(), second.Start(), first.ID(), position)
		}
		replay := make([]byte, len(seen)-at)
		if _, err := io.ReadFull(second, replay); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(replay, seen[at:]) {
			t.Fatalf("replay = %q, want %q", replay, seen[at:])
		}
		rest := Watch(t, second)
		send(t, second, `echo three""3`)
		rest.WaitFor("three3")
	})

	t.Run("terminal resume after eviction reports lost output", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		out := Watch(t, first)
		send(t, first, "seq 1 "+floodLines)
		send(t, first, `echo flood""done`)
		out.WaitFor("flooddone")
		_ = first.Close()
		_, _ = out.Stop()
		if first.Offset() <= execenv.TerminalHistoryBytes {
			t.Fatalf("flood produced only %d bytes", first.Offset())
		}
		beyond := resumeAt(first, first.Offset()+1<<30)
		if _, err := env.Attach(t.Context(), beyond); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("resume beyond output = %v, want ErrInvalid", err)
		}
		second := attachTerminal(t, env, resumeAt(first, 0))
		if second.Start() == 0 || second.Offset() != second.Start() {
			t.Fatalf("resume of overwritten output Start=%d Offset=%d, want Start > 0", second.Start(), second.Offset())
		}
		rest := Watch(t, second)
		send(t, second, `echo after""flood`)
		rest.WaitFor("afterflood")
	})

	t.Run("terminal resume of another session is not found", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		_ = first.Close()
		unknown := execenv.AttachRequest{Resume: &execenv.TerminalPosition{Terminal: "execenv-unknown"}}
		if _, err := env.Attach(t.Context(), unknown); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("resume unknown = %v, want ErrNotFound", err)
		}
	})

	t.Run("terminal attach is busy without takeover", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		if _, err := env.Attach(t.Context(), execenv.AttachRequest{}); !errors.Is(err, execenv.ErrBusy) {
			t.Fatalf("second Attach = %v, want ErrBusy", err)
		}
		if _, err := env.Attach(t.Context(), resumeAt(first, 0)); !errors.Is(err, execenv.ErrBusy) {
			t.Fatalf("resume while attached = %v, want ErrBusy", err)
		}
	})

	t.Run("terminal takeover supersedes the attached client", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		out := Watch(t, first)
		second := attachTerminal(t, env, execenv.AttachRequest{Takeover: true})
		if second.ID() != first.ID() {
			t.Fatal("takeover started another shell")
		}
		if _, err := out.Stop(); !errors.Is(err, execenv.ErrSuperseded) {
			t.Fatalf("replaced Read = %v, want ErrSuperseded", err)
		}
		if _, err := first.Write([]byte("x")); !errors.Is(err, execenv.ErrSuperseded) {
			t.Fatalf("replaced Write = %v, want ErrSuperseded", err)
		}
		rest := Watch(t, second)
		send(t, second, `echo take""over`)
		rest.WaitFor("takeover")
	})

	t.Run("terminal detach keeps the shell", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		out := Watch(t, first)
		send(t, first, "EXECENV_KEPT=still")
		send(t, first, `echo set""1`)
		out.WaitFor("set1")
		_ = first.Close()
		_, _ = out.Stop()
		second := attachTerminal(t, env, execenv.AttachRequest{Window: execenv.Window{Cols: 100, Rows: 30}})
		if second.ID() != first.ID() {
			t.Fatal("reattach started another shell")
		}
		if second.Start() > first.Offset() {
			t.Fatalf("tail Start = %d past the detached Offset %d", second.Start(), first.Offset())
		}
		rest := Watch(t, second)
		send(t, second, "echo $EXECENV_KEPT-x")
		rest.WaitFor("still-x")
	})

	t.Run("terminal grace expiry ends the session", func(t *testing.T) {
		const grace = 100 * time.Millisecond
		env := ensureGrant(t, factory(t, Config{TerminalGrace: grace}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		_ = first.Close()
		gone := false
		for range 20 {
			time.Sleep(3 * grace)
			term, err := env.Attach(t.Context(), resumeAt(first, 0))
			if errors.Is(err, execenv.ErrNotFound) {
				gone = true
				break
			}
			if err != nil {
				t.Fatalf("resume during grace = %v", err)
			}
			// Attaching restarted the grace; detach and wait again.
			_ = term.Close()
		}
		if !gone {
			t.Fatal("detached shell outlived its grace")
		}
		next := attachTerminal(t, env, execenv.AttachRequest{})
		if next.ID() == first.ID() || next.Start() != 0 {
			t.Fatalf("attach after grace ID=%q Start=%d, want a new shell", next.ID(), next.Start())
		}
	})

	t.Run("terminal hangup ends the session now", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		if err := first.Hangup(t.Context()); err != nil {
			t.Fatalf("Hangup() error = %v", err)
		}
		if _, err := first.Write([]byte("x")); err == nil {
			t.Fatal("Write after Hangup succeeded")
		}
		if _, err := env.Attach(t.Context(), resumeAt(first, 0)); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("resume after hangup = %v, want ErrNotFound", err)
		}
		next := attachTerminal(t, env, execenv.AttachRequest{})
		if next.ID() == first.ID() {
			t.Fatal("hangup kept the shell")
		}
	})

	t.Run("terminal shell exit ends the session", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		out := Watch(t, first)
		send(t, first, "exit")
		if _, err := out.Stop(); err != io.EOF {
			t.Fatalf("Read after exit = %v, want io.EOF", err)
		}
		if _, err := env.Attach(t.Context(), resumeAt(first, 0)); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("resume after exit = %v, want ErrNotFound", err)
		}
		next := attachTerminal(t, env, execenv.AttachRequest{})
		if next.ID() == first.ID() || next.Start() != 0 {
			t.Fatalf("attach after exit ID=%q Start=%d, want a new shell", next.ID(), next.Start())
		}
	})

	t.Run("terminal freeze and thaw keep the session", func(t *testing.T) {
		env := ensureGrant(t, factory(t, Config{}))
		first := attachTerminal(t, env, execenv.AttachRequest{})
		out := Watch(t, first)
		send(t, first, "EXECENV_FROZEN=thawed")
		send(t, first, `echo set""2`)
		out.WaitFor("set2")
		fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlFrozen}); err != nil {
			t.Fatalf("Control(frozen) error = %v", err)
		}
		if _, err := first.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("frozen Write = %v, want ErrFrozen", err)
		}
		// Detaching while frozen keeps the session too.
		_ = first.Close()
		_, _ = out.Stop()
		fence.FenceRevision++
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
			t.Fatalf("Control(running) error = %v", err)
		}
		second := attachTerminal(t, env, resumeAt(first, first.Offset()))
		if second.ID() != first.ID() || second.Start() != first.Offset() {
			t.Fatalf("resume after thaw ID=%q Start=%d, want %q at %d", second.ID(), second.Start(), first.ID(), first.Offset())
		}
		rest := Watch(t, second)
		send(t, second, "echo $EXECENV_FROZEN-x")
		rest.WaitFor("thawed-x")
	})
}
