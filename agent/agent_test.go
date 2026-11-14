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
)

func TestClosePtyIsHangup(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cli.Verify(t.Context()); err != nil {
		t.Fatalf("link lost on hangup: %v", err)
	}
	again, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatalf("Attach after hangup: %v", err)
	}
	_ = again.Close()
}

func TestQuiesceStopsIO(t *testing.T) {
	t.Parallel()
	cli, _ := startAgent(t)
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if err := cli.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("x")); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("Write after quiesce = %v, want ErrFrozen", err)
	}
	if _, err := cli.Attach(t.Context(), execenv.Window{}); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("Attach after quiesce = %v, want ErrFrozen", err)
	}
	if err := cli.Reopen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("echo retained\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Attach(t.Context(), execenv.Window{}); !errors.Is(err, execenv.ErrBusy) {
		t.Fatalf("second attach: %v", err)
	}
	_ = term.Close()
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
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte("touch seen.txt\n")); err != nil {
		t.Fatal(err)
	}
	waitObservation(t, cli, fence, execenv.ObservationCreateFile, "seen.txt")
}

func TestQuiesceReopenPreservesShellProcessAndState(t *testing.T) {
	cli, home := startAgent(t)
	if err := cli.Verify(t.Context()); err != nil {
		t.Fatal(err)
	}
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	// Have the shell record its actual PID and an unexported variable. A newly
	// attached shell cannot reconstruct this state from the projection.
	if _, err := term.Write([]byte("retained=proof; printf '%s' \"$$\" > before-pid\n")); err != nil {
		t.Fatal(err)
	}
	before := waitFile(t, filepath.Join(home, "before-pid"))
	if err := cli.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := cli.Reopen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("printf '%s:%s' \"$$\" \"$retained\" > after-state\n")); err != nil {
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

func TestCloseWhileQuiescedIsDeferredUntilReopen(t *testing.T) {
	cli, _ := startAgent(t)
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("closing paused terminal waited for guest RPC")
	}
	if err := cli.Reopen(t.Context()); err != nil {
		t.Fatal(err)
	}
	replacement, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatalf("closed stream still occupied terminal: %v", err)
	}
	defer replacement.Close()
	if err := term.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Write([]byte("echo successor\n")); err != nil {
		t.Fatalf("old close affected replacement: %v", err)
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
	term, err := cli.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte("mkdir -p \"$HOME/.cache\" && echo cached > \"$HOME/.cache/x\" && printf '%s\\n%s\\n' \"$HOME\" \"$(pwd -P)\" > where.txt\n")); err != nil {
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
