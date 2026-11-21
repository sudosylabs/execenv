package execenvtest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// TestShell is a terminal process the suite types into.
type TestShell struct {
	t      *testing.T
	env    execenv.Env
	fence  execenv.ProjectionFence
	tag    string
	input  uint64
	a      execenv.Attachment
	seen   strings.Builder
	offset uint64
}

// StartShell starts a login shell tagged tag and attaches to it.
func StartShell(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, tag string) *TestShell {
	t.Helper()
	if _, err := env.Start(t.Context(), fence, execenv.ProcessSpec{Tag: tag, PTY: &execenv.Window{Cols: 80, Rows: 24}}); err != nil {
		t.Fatalf("Start(shell) error = %v", err)
	}
	a, err := env.AttachProcess(t.Context(), tag, nil)
	if err != nil {
		t.Fatalf("AttachProcess() error = %v", err)
	}
	s := &TestShell{t: t, env: env, fence: fence, tag: tag, a: a}
	t.Cleanup(func() { _ = a.Close() })
	return s
}

// Type sends line and a newline at the shell's input offset.
func (s *TestShell) Type(line string) error {
	next, err := s.env.WriteInput(s.t.Context(), s.fence, s.tag, s.input, []byte(line+"\n"))
	if err == nil {
		s.input = next
	}
	return err
}

// Expect reads until marker appears in the output.
func (s *TestShell) Expect(marker string) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), 5*time.Second)
	defer cancel()
	for !strings.Contains(s.seen.String(), marker) {
		out, err := s.a.Next(ctx)
		if err != nil {
			s.t.Fatalf("waiting for %q: %v (saw %q)", marker, err, s.seen.String())
		}
		if out.Kind == execenv.OutputData {
			s.seen.Write(out.Data)
			s.offset = out.Offset + uint64(len(out.Data))
		}
	}
}

// Seen is the output read so far; Offset is where it ends.
func (s *TestShell) Seen() string   { return s.seen.String() }
func (s *TestShell) Offset() uint64 { return s.offset }

// Attachment is the shell's reader.
func (s *TestShell) Attachment() execenv.Attachment { return s.a }

// drain reads an attachment to its end.
func drain(ctx context.Context, a execenv.Attachment) (map[execenv.Stream]string, execenv.ExitStatus, error) {
	out := map[execenv.Stream]string{}
	for {
		ev, err := a.Next(ctx)
		if err != nil {
			return out, execenv.ExitStatus{}, err
		}
		switch ev.Kind {
		case execenv.OutputData:
			out[ev.Stream] += string(ev.Data)
		case execenv.OutputExit:
			return out, ev.Exit, nil
		}
	}
}

func run(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, tag string, argv ...string) (map[execenv.Stream]string, execenv.ExitStatus) {
	t.Helper()
	if _, err := env.Start(t.Context(), fence, execenv.ProcessSpec{Tag: tag, Argv: argv}); err != nil {
		t.Fatalf("Start(%v) error = %v", argv, err)
	}
	a, err := env.AttachProcess(t.Context(), tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out, exit, err := drain(ctx, a)
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return out, exit
}

func runProcessAPI(t *testing.T, factory Factory) {
	t.Run("a command runs to its exit code and its output stays readable", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "commands")
		out, exit := run(t, env, fence, "hello", "echo", "hello")
		if strings.TrimSpace(out[execenv.StreamStdout]) != "hello" || exit != (execenv.ExitStatus{}) {
			t.Fatalf("echo hello = %q %+v", out, exit)
		}
		_, exit = run(t, env, fence, "fails", "false")
		if exit.Code != 1 {
			t.Fatalf("false exited %+v, want code 1", exit)
		}
		info, err := env.Process(t.Context(), "hello")
		if err != nil || info.Running || info.Output[execenv.StreamStdout] == 0 {
			t.Fatalf("Process(hello) = %+v, %v", info, err)
		}
		again, err := env.AttachProcess(t.Context(), "hello", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer again.Close()
		out, _, err = drain(t.Context(), again)
		if err != nil || strings.TrimSpace(out[execenv.StreamStdout]) != "hello" {
			t.Fatalf("re-reading an exited command = %q %v", out, err)
		}
	})

	t.Run("processes report the retained exit time", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "exit-time")
		sh := StartShell(t, env, fence, "clock")
		running, err := env.Process(t.Context(), "clock")
		if err != nil || !running.Running || running.Started.IsZero() || !running.Ended.IsZero() {
			t.Fatalf("running Process(clock) = %+v, %v", running, err)
		}
		if err := sh.Type("exit 7"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if _, exit, err := drain(ctx, sh.Attachment()); err != nil || exit.Code != 7 {
			t.Fatalf("shell exit = %+v, %v", exit, err)
		}
		ended, err := env.Process(ctx, "clock")
		if err != nil || ended.Running || ended.Ended.IsZero() || ended.Ended.Before(running.Started) || !ended.Started.Equal(running.Started) {
			t.Fatalf("exited Process(clock) = %+v, %v; started at %v", ended, err, running.Started)
		}
		listed, err := env.ListProcesses(ctx)
		if err != nil || len(listed) != 1 || !listed[0].Ended.Equal(ended.Ended) {
			t.Fatalf("ListProcesses() = %+v, %v; want original exit time %v", listed, err, ended.Ended)
		}
	})

	t.Run("a terminal's screen redraws it and continues from its offset", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "screen")
		sh := StartShell(t, env, fence, "sh")
		if err := sh.Type(`echo scr""een-check`); err != nil {
			t.Fatal(err)
		}
		sh.Expect("screen-check")
		var s execenv.Screen
		deadline := time.Now().Add(5 * time.Second)
		for {
			var err error
			s, err = env.Screen(t.Context(), "sh")
			if err != nil {
				t.Fatal(err)
			}
			if s.Offset >= sh.offset {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("screen offset %d behind output %d", s.Offset, sh.offset)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if s.Window != (execenv.Window{Cols: 80, Rows: 24}) || len(s.Text) != 24 || len(s.Data) == 0 {
			t.Fatalf("screen = %+v", s)
		}
		if !slices.ContainsFunc(s.Text, func(row string) bool { return strings.Contains(row, "screen-check") }) {
			t.Fatalf("screen text %q lacks the output", s.Text)
		}
		// Attaching from the screen's offset continues it: what follows is
		// only what came after.
		a, err := env.AttachProcess(t.Context(), "sh", map[execenv.Stream]uint64{execenv.StreamPTY: s.Offset})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if err := sh.Type(`echo aft""er-screen`); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var seen strings.Builder
		for !strings.Contains(seen.String(), "after-screen") {
			ev, err := a.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind == execenv.OutputData {
				if ev.Offset < s.Offset {
					t.Fatalf("continued from %d before the screen's %d", ev.Offset, s.Offset)
				}
				seen.Write(ev.Data)
			}
		}
		if strings.Contains(seen.String(), "screen-check") {
			t.Fatal("output the screen already showed came again")
		}
		if err := env.Resize(t.Context(), fence, "sh", execenv.Window{Cols: 100, Rows: 30}); err != nil {
			t.Fatal(err)
		}
		if s, err := env.Screen(t.Context(), "sh"); err != nil || s.Window != (execenv.Window{Cols: 100, Rows: 30}) {
			t.Fatalf("Screen() after resize = %+v, %v", s.Window, err)
		}
		if _, err := env.Start(t.Context(), fence, execenv.ProcessSpec{Tag: "plain", Argv: []string{"true"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.Screen(t.Context(), "plain"); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("Screen() of a process without a terminal = %v, want ErrInvalid", err)
		}
		if _, err := env.Screen(t.Context(), "nobody"); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("Screen() of no process = %v, want ErrNotFound", err)
		}
	})

	t.Run("a shell resumes by offset for any number of readers", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "resume")
		sh := StartShell(t, env, fence, "sh")
		// A terminal echoes what is typed: the quotes keep the echo from
		// matching the output.
		if err := sh.Type(`echo fir""st`); err != nil {
			t.Fatal(err)
		}
		sh.Expect("first")
		mark := sh.offset
		second, err := env.AttachProcess(t.Context(), "sh", map[execenv.Stream]uint64{execenv.StreamPTY: mark})
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		if err := sh.Type(`echo aft""er`); err != nil {
			t.Fatal(err)
		}
		sh.Expect("after")
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var seen strings.Builder
		for !strings.Contains(seen.String(), "after") {
			ev, err := second.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind == execenv.OutputData {
				if ev.Offset < mark {
					t.Fatalf("resumed reader got offset %d before %d", ev.Offset, mark)
				}
				seen.Write(ev.Data)
			}
		}
		if strings.Contains(seen.String(), "first") {
			t.Fatal("a reader resumed past output it already had got it again")
		}
		if _, err := env.AttachProcess(t.Context(), "sh", map[execenv.Stream]uint64{execenv.StreamPTY: 1 << 40}); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("AttachProcess() beyond the output = %v, want ErrInvalid", err)
		}
	})

	t.Run("input is taken once by offset", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "input")
		sh := StartShell(t, env, fence, "sh")
		line := []byte("echo once\n")
		next, err := env.WriteInput(t.Context(), fence, "sh", 0, line)
		if err != nil || next != uint64(len(line)) {
			t.Fatalf("WriteInput() = %d, %v", next, err)
		}
		// A resend of the same bytes is dropped, not typed twice.
		if again, err := env.WriteInput(t.Context(), fence, "sh", 0, line); err != nil || again != next {
			t.Fatalf("resend = %d, %v", again, err)
		}
		if _, err := env.WriteInput(t.Context(), fence, "sh", next+5, []byte("x")); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("WriteInput() past the input = %v, want ErrConflict", err)
		}
		sh.Expect("once")
		info, err := env.Process(t.Context(), "sh")
		if err != nil || info.Input != next {
			t.Fatalf("Process().Input = %+v, %v", info, err)
		}
	})

	t.Run("a tag runs once, and signals and timeouts end processes", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "signals")
		StartShell(t, env, fence, "sh")
		if _, err := env.Start(t.Context(), fence, execenv.ProcessSpec{Tag: "sh", PTY: &execenv.Window{}}); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("Start() of a running tag = %v, want ErrConflict", err)
		}
		if err := env.SignalProcess(t.Context(), fence, "sh", execenv.SignalKill); err != nil {
			t.Fatal(err)
		}
		waitExited(t, env, "sh")
		if info, _ := env.Process(t.Context(), "sh"); info.Exit.Signal != 9 {
			t.Fatalf("killed shell exit = %+v, want signal 9", info.Exit)
		}
		if _, err := env.Start(t.Context(), fence, execenv.ProcessSpec{Tag: "sh", PTY: &execenv.Window{}}); err != nil {
			t.Fatalf("Start() of an exited tag = %v", err)
		}
		if _, err := env.Start(t.Context(), fence, execenv.ProcessSpec{Tag: "slow", PTY: &execenv.Window{}, Timeout: 100 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		waitExited(t, env, "slow")
		if _, err := env.Process(t.Context(), "nobody"); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("Process(unknown) = %v, want ErrNotFound", err)
		}
	})

	t.Run("a shell nothing attaches to ends after its grace", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "grace")
		if _, err := env.Start(t.Context(), fence, execenv.ProcessSpec{Tag: "sh", PTY: &execenv.Window{}, DetachGrace: 100 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		a, err := env.AttachProcess(t.Context(), "sh", nil)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		if info, _ := env.Process(t.Context(), "sh"); !info.Running {
			t.Fatal("an attached shell ended for its grace")
		}
		_ = a.Close()
		waitExited(t, env, "sh")
	})

	t.Run("a frozen grant takes no input and starts nothing", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "frozen")
		sh := StartShell(t, env, fence, "sh")
		frozen := fence
		frozen.FenceRevision++
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: frozen, State: execenv.ControlFrozen}); err != nil {
			t.Fatal(err)
		}
		if _, err := env.WriteInput(t.Context(), frozen, "sh", 0, []byte("x")); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("WriteInput() while frozen = %v, want ErrFrozen", err)
		}
		if _, err := env.Start(t.Context(), frozen, execenv.ProcessSpec{Tag: "more", Argv: []string{"true"}}); !errors.Is(err, execenv.ErrFrozen) {
			t.Fatalf("Start() while frozen = %v, want ErrFrozen", err)
		}
		thawed := frozen
		thawed.FenceRevision++
		if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: thawed, State: execenv.ControlRunning}); err != nil {
			t.Fatal(err)
		}
		sh.fence = thawed
		if err := sh.Type("echo thawed"); err != nil {
			t.Fatal(err)
		}
		sh.Expect("thawed")
	})
}

func waitExited(t *testing.T, env execenv.Env, tag string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		info, err := env.Process(t.Context(), tag)
		if err == nil && !info.Running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not exit", tag)
}
