package agent_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/internal/procs"
)

// testShell is a login shell in the guest, driven through the host's
// process manager as a grant drives it.
type testShell struct {
	t     *testing.T
	m     *procs.Manager
	tag   string
	input uint64
	a     execenv.Attachment
	seen  strings.Builder
}

func startShell(t *testing.T, cli *agent.Client) *testShell {
	t.Helper()
	m := procs.New(cli.Start)
	t.Cleanup(func() { m.End(execenv.ErrClosed) })
	return shellOn(t, m, "sh")
}

func shellOn(t *testing.T, m *procs.Manager, tag string) *testShell {
	t.Helper()
	if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: tag, PTY: &execenv.Window{Cols: 80, Rows: 24}}); err != nil {
		t.Fatalf("Start(shell) = %v", err)
	}
	a, err := m.Attach(tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return &testShell{t: t, m: m, tag: tag, a: a}
}

func (s *testShell) typeLine(line string) error {
	next, err := s.m.WriteInput(s.tag, s.input, []byte(line+"\n"))
	if err == nil {
		s.input = next
	}
	return err
}

func (s *testShell) expect(marker string) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), 5*time.Second)
	defer cancel()
	for !strings.Contains(s.seen.String(), marker) {
		out, err := s.a.Next(ctx)
		if err != nil {
			s.t.Fatalf("waiting for %q: %v (saw %q)", marker, err, s.seen.String())
		}
		s.seen.Write(out.Data)
	}
}

func waitExit(t *testing.T, m *procs.Manager, tag string) execenv.ExitStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := m.Process(tag); err == nil && !info.Running {
			return info.Exit
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not exit", tag)
	return execenv.ExitStatus{}
}

func TestShellEndsOnHangupAndItsTagRunsAgain(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	sh := startShell(t, cli)
	if err := sh.typeLine("echo hello"); err != nil {
		t.Fatal(err)
	}
	sh.expect("hello")
	if err := sh.m.Signal("sh", execenv.SignalHangup); err != nil {
		t.Fatal(err)
	}
	if exit := waitExit(t, sh.m, "sh"); exit.Signal != 1 {
		t.Fatalf("hung-up shell exit = %+v, want signal 1", exit)
	}
	if err := cli.Verify(t.Context()); err != nil {
		t.Fatalf("link lost on hangup: %v", err)
	}
	again := shellOn(t, sh.m, "sh")
	if err := again.typeLine("echo again"); err != nil {
		t.Fatal(err)
	}
	again.expect("again")
}

// A command without a terminal has separate outputs, optional input and
// its own exit code.
func TestCommandsRunOnPipes(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	m := procs.New(cli.Start)
	t.Cleanup(func() { m.End(execenv.ErrClosed) })
	if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "c", Argv: []string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"}}); err != nil {
		t.Fatal(err)
	}
	if exit := waitExit(t, m, "c"); exit.Code != 3 {
		t.Fatalf("exit = %+v, want code 3", exit)
	}
	got := drain(t, m, "c")
	if got[execenv.StreamStdout] != "out\n" || got[execenv.StreamStderr] != "err\n" {
		t.Fatalf("outputs = %q", got)
	}
	if _, err := m.Start(t.Context(), execenv.ProcessSpec{Tag: "cat", Argv: []string{"cat"}, Stdin: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WriteInput("cat", 0, []byte("piped")); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseInput("cat"); err != nil {
		t.Fatal(err)
	}
	if exit := waitExit(t, m, "cat"); exit != (execenv.ExitStatus{}) {
		t.Fatalf("cat exit = %+v", exit)
	}
	if got := drain(t, m, "cat"); got[execenv.StreamStdout] != "piped" {
		t.Fatalf("cat output = %q", got)
	}
}

func drain(t *testing.T, m *procs.Manager, tag string) map[execenv.Stream]string {
	t.Helper()
	a, err := m.Attach(tag, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out := map[execenv.Stream]string{}
	for {
		ev, err := a.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == execenv.OutputExit {
			return out
		}
		out[ev.Stream] += string(ev.Data)
	}
}

func TestQuiesceStopsIO(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	sh := startShell(t, cli)
	if err := cli.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Start(t.Context(), execenv.ProcessSpec{Tag: "more", Argv: []string{"true"}}); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("Start after quiesce = %v, want ErrFrozen", err)
	}
	if err := cli.Reopen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := sh.typeLine("echo retained"); err != nil {
		t.Fatal(err)
	}
	sh.expect("retained")
}

func TestListenGuestIsGuestOnly(t *testing.T) {
	t.Parallel()
	ln, err := agent.ListenGuest()
	if err == nil {
		_ = ln.Close()
		if ln == nil {
			t.Fatal("ListenGuest() returned a nil listener")
		}
		return
	}
	if !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("ListenGuest() error = %v, want ErrUnavailable off a guest", err)
	}
}

func startAgent(t *testing.T) (*agent.Client, string) {
	t.Helper()
	home := t.TempDir()
	host, guest := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errc := make(chan error, 1)
	go func() {
		errc <- agent.Serve(ctx, guest, agent.Config{Home: home, Barrier: quiescent{}, Processes: newFakeProcesses()})
	}()
	t.Cleanup(func() {
		_ = host.Close()
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
		}
	})
	return agent.NewClient(host), home
}

// quiescent is a test write barrier. The shell in these tests is the only
// writer and the tests wait for its output before observing.
type quiescent struct{}

func (quiescent) Stopped(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// waitObservation polls Observe until an observation of op at path appears.
func waitObservation(t *testing.T, cli *agent.Client, fence execenv.ProjectionFence, op execenv.ObservationOperation, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page, err := cli.Observe(t.Context(), fence, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Observations {
			if event.Operation == op && event.Path == path {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s observation of %s", op, path)
}

// initialize opens the journal with an empty projection.
func initialize(t *testing.T, cli *agent.Client) execenv.ProjectionFence {
	t.Helper()
	if err := cli.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	fence := execenv.ProjectionFence{ExecutionGrantID: "grant", EnvironmentEpoch: "epoch", FenceRevision: 1}
	if _, err := cli.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init"}); err != nil {
		t.Fatal(err)
	}
	return fence
}

func TestTouchInPtyBecomesObservation(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	fence := initialize(t, cli)
	sh := startShell(t, cli)
	if err := sh.typeLine("touch seen.txt"); err != nil {
		t.Fatal(err)
	}
	waitObservation(t, cli, fence, execenv.ObservationCreateFile, "seen.txt")
}

func TestQuiesceReopenPreservesShellProcessAndState(t *testing.T) {
	cli, home := startAgent(t)
	if err := cli.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	sh := startShell(t, cli)
	// Have the shell record its actual PID and an unexported variable. A newly
	// started shell cannot reconstruct this state from the projection.
	if err := sh.typeLine("retained=proof; printf '%s' \"$$\" > before-pid"); err != nil {
		t.Fatal(err)
	}
	before := waitFile(t, filepath.Join(home, "before-pid"))
	if err := cli.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cli.Reopen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := sh.typeLine("printf '%s:%s' \"$$\" \"$retained\" > after-state"); err != nil {
		t.Fatal(err)
	}
	after := waitFile(t, filepath.Join(home, "after-state"))
	if string(after) != fmt.Sprintf("%s:proof", before) {
		t.Fatalf("shell state lost: before=%q after=%q", before, after)
	}
}

func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
			return body
		}
		select {
		case <-deadline.C:
			t.Fatal("shell output file not written")
			return nil
		case <-tick.C:
		}
	}
}

func TestShellHomeIsScratchOutsideProjectedTree(t *testing.T) {
	home, scratch := t.TempDir(), t.TempDir()
	host, guest := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = agent.Serve(ctx, guest, agent.Config{Home: home, Scratch: scratch, Barrier: quiescent{}, Processes: newFakeProcesses()})
	}()
	t.Cleanup(func() { _ = host.Close() })
	cli := agent.NewClient(host)
	fence := initialize(t, cli)
	sh := startShell(t, cli)
	if err := sh.typeLine("mkdir -p \"$HOME/.cache\" && echo cached > \"$HOME/.cache/x\" && printf '%s\\n%s\\n' \"$HOME\" \"$(pwd -P)\" > where.txt"); err != nil {
		t.Fatal(err)
	}
	waitObservation(t, cli, fence, execenv.ObservationCreateFile, "where.txt")
	body := strings.Split(strings.TrimSpace(string(waitFile(t, filepath.Join(home, "where.txt")))), "\n")
	realHome, _ := filepath.EvalSymlinks(home)
	if len(body) != 2 || body[0] != scratch || body[1] != realHome {
		t.Fatalf("HOME/cwd = %q, want %q and %q", body, scratch, realHome)
	}
	if _, err := os.Stat(filepath.Join(scratch, ".cache", "x")); err != nil {
		t.Fatalf("cache not written to scratch HOME: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".cache")); !os.IsNotExist(err) {
		t.Fatalf("cache leaked into projected tree: %v", err)
	}
	page, err := cli.Observe(t.Context(), fence, 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Observations {
		if strings.HasPrefix(event.Path, ".cache") {
			t.Fatalf("scratch HOME observed: %+v", event)
		}
	}
}

func TestHelperWithoutJournalIsRefused(t *testing.T) {
	t.Parallel()
	host, guest := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = agent.Serve(ctx, guest, agent.Config{Home: t.TempDir(), Processes: newFakeProcesses()}) }()
	t.Cleanup(func() { _ = host.Close() })
	if err := agent.NewClient(host).Verify(t.Context()); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("helper without a journal accepted: %v", err)
	}
}

func TestHelperWithoutProcessLogIsRefused(t *testing.T) {
	t.Parallel()
	host, guest := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = agent.Serve(ctx, guest, agent.Config{Home: t.TempDir(), Barrier: quiescent{}}) }()
	t.Cleanup(func() { _ = host.Close() })
	if err := agent.NewClient(host).Verify(t.Context()); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("helper without a process log accepted: %v", err)
	}
}

// The helper sends each event as it is taken and bounds every source's
// command lines; the host retains and sequences them.
func TestProcessEventsReachTheHostBounded(t *testing.T) {
	t.Parallel()
	source := newFakeProcesses()
	host, guest := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = agent.Serve(ctx, guest, agent.Config{Home: t.TempDir(), Barrier: quiescent{}, Processes: source})
	}()
	t.Cleanup(func() { _ = host.Close() })
	cli := agent.NewClient(host)
	if err := cli.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	long := make([]string, 80)
	for i := range long {
		long[i] = "arg"
	}
	code := 0
	source.events <- execenv.ProcessEvent{Kind: execenv.ProcessStarted, PID: 7, ParentPID: 1, Argv: long, At: time.Now()}
	source.events <- execenv.ProcessEvent{Kind: execenv.ProcessExited, PID: 7, ExitCode: &code, At: time.Now()}
	deadline := time.Now().Add(5 * time.Second)
	for {
		page, err := cli.ObserveProcesses(t.Context(), 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) == 2 {
			started := page.Events[0]
			if started.Sequence != 1 || len(started.Argv) != execenv.MaxProcessArgs || !started.Truncated || page.Events[1].Kind != execenv.ProcessExited || page.Next != 2 {
				t.Fatalf("events: %+v", page.Events)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events never arrived: %+v", page)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := cli.AcknowledgeProcesses(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.ObserveProcesses(t.Context(), 1, 10); !errors.Is(err, execenv.ErrLagged) {
		t.Fatalf("acknowledged prefix: %v", err)
	}
}

// fakeProcesses is a test process source fed by the test.
type fakeProcesses struct {
	events chan execenv.ProcessEvent
	done   chan struct{}
	once   sync.Once
}

func newFakeProcesses() *fakeProcesses {
	return &fakeProcesses{events: make(chan execenv.ProcessEvent), done: make(chan struct{})}
}

func (f *fakeProcesses) Next() (execenv.ProcessEvent, error) {
	select {
	case event := <-f.events:
		return event, nil
	case <-f.done:
		return execenv.ProcessEvent{}, execenv.ErrClosed
	}
}

func (f *fakeProcesses) Close() error {
	f.once.Do(func() { close(f.done) })
	return nil
}
