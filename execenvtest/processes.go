package execenvtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// Workload runs commands in one environment's contained workload as its own
// processes would.
type Workload interface {
	// Run starts argv once. It need not wait for the command to end; the
	// suite waits for the process log instead.
	Run(ctx context.Context, argv []string) error
	// Start runs argv once as a process of its own under tag, as the
	// environment's Start does.
	Start(ctx context.Context, tag string, argv []string) error
}

// Traced is one running environment, the fence that drives it, and a
// workload acting inside it. The workload must start nothing else while a
// subtest runs, apart from processes that are running before its first
// command.
type Traced struct {
	Env      execenv.Env
	Fence    execenv.ProjectionFence
	Workload Workload
}

// TracedFactory returns a fresh Traced for one subtest.
type TracedFactory func(t *testing.T) Traced

// ShellWorkload runs each command by typing it into a shell started in env
// and discards the output. The suite's commands are /bin/true with plain
// words, so a real shell starts exactly that program with exactly those
// words.
func ShellWorkload(t *testing.T, env execenv.Env, fence execenv.ProjectionFence) Workload {
	t.Helper()
	sh := StartShell(t, env, fence, "workload")
	go func() {
		for {
			if _, err := sh.a.Next(context.Background()); err != nil {
				return
			}
		}
	}()
	return shellWorkload{sh: sh}
}

type shellWorkload struct{ sh *TestShell }

func (w shellWorkload) Run(_ context.Context, argv []string) error {
	return w.sh.Type(strings.Join(argv, " "))
}

func (w shellWorkload) Start(ctx context.Context, tag string, argv []string) error {
	_, err := w.sh.env.Start(ctx, w.sh.fence, execenv.ProcessSpec{Tag: tag, Argv: argv})
	return err
}

// RunProcesses exercises the process log against a workload. It needs a
// real or simulated guest, so it is separate from Run.
func RunProcesses(t *testing.T, factory TracedFactory) {
	t.Helper()

	t.Run("commands enter the log as ordered start and exit pairs", func(t *testing.T) {
		p := startTraced(t, factory)
		base := p.settle()
		commands := [][]string{{"/bin/true", "first"}, {"/bin/true", "second"}, {"/bin/true", "third"}}
		for _, argv := range commands {
			p.run(argv)
		}
		events := p.collect(base, func(events []execenv.ProcessEvent) bool {
			_, ok := pairs(events, commands)
			return ok
		})
		found, _ := pairs(events, commands)
		for i, pair := range found {
			start, exit := events[pair[0]], events[pair[1]]
			if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.Signal != 0 {
				t.Fatalf("%v exited %+v, want status 0", commands[i], exit)
			}
			if start.Truncated || start.Unread || start.At.IsZero() {
				t.Fatalf("%v started %+v", commands[i], start)
			}
			if i > 0 && pair[0] < found[i-1][0] {
				t.Fatalf("%v started before %v", commands[i], commands[i-1])
			}
		}
	})

	t.Run("a started process carries its tag and its commands none", func(t *testing.T) {
		p := startTraced(t, factory)
		base := p.settle()
		// One at a time: the shell runs its command when it gets to it.
		check := func(argv []string, start func(), want string) {
			t.Helper()
			start()
			events := p.collect(base, func(events []execenv.ProcessEvent) bool {
				_, ok := pairs(events, [][]string{argv})
				return ok
			})
			found, _ := pairs(events, [][]string{argv})
			if tag := events[found[0][0]].Tag; tag != want {
				t.Fatalf("%v started with tag %q, want %q", argv, tag, want)
			}
		}
		command := []string{"/bin/true", "untagged"}
		check(command, func() { p.run(command) }, "")
		tagged := []string{"/bin/true", "tagged"}
		check(tagged, func() {
			if err := p.workload.Start(p.t.Context(), "tagged", tagged); err != nil {
				t.Fatal(err)
			}
		}, "tagged")
	})

	t.Run("acknowledgement trims the log", func(t *testing.T) {
		p := startTraced(t, factory)
		base := p.settle()
		if base == 0 {
			t.Fatal("settled log is empty")
		}
		if _, err := p.env.ObserveProcesses(t.Context(), p.fence, 0, execenv.MaxProcessPage); !errors.Is(err, execenv.ErrLagged) {
			t.Fatalf("ObserveProcesses(acknowledged) error = %v, want ErrLagged", err)
		}
		if err := p.env.AcknowledgeProcesses(t.Context(), p.fence, base-1); err != nil {
			t.Fatalf("older AcknowledgeProcesses() error = %v", err)
		}
		page, err := p.env.ObserveProcesses(t.Context(), p.fence, base, execenv.MaxProcessPage)
		if err != nil || page.Next < base {
			t.Fatalf("ObserveProcesses(next) = %+v, %v", page, err)
		}
		if err := p.env.AcknowledgeProcesses(t.Context(), p.fence, page.Next+1000); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("AcknowledgeProcesses(future) error = %v, want ErrInvalid", err)
		}
		if _, err := p.env.ObserveProcesses(t.Context(), p.fence, page.Next+1000, execenv.MaxProcessPage); !errors.Is(err, execenv.ErrInvalid) {
			t.Fatalf("ObserveProcesses(future) error = %v, want ErrInvalid", err)
		}
		limits := []int{0, execenv.MaxProcessPage + 1}
		wide := int64(1)<<32 | 1
		if int64(int(wide)) == wide {
			limits = append(limits, int(wide))
		}
		for _, limit := range limits {
			if _, err := p.env.ObserveProcesses(t.Context(), p.fence, base, limit); !errors.Is(err, execenv.ErrInvalid) {
				t.Fatalf("ObserveProcesses(limit %d) error = %v, want ErrInvalid", limit, err)
			}
		}
	})

	t.Run("a stale fence is refused", func(t *testing.T) {
		p := startTraced(t, factory)
		stale := p.fence
		stale.EnvironmentEpoch = "stale-epoch"
		if _, err := p.env.ObserveProcesses(t.Context(), stale, 0, 1); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("ObserveProcesses(stale epoch) error = %v, want ErrConflict", err)
		}
		if err := p.env.AcknowledgeProcesses(t.Context(), stale, 0); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("AcknowledgeProcesses(stale epoch) error = %v, want ErrConflict", err)
		}
		newer := p.fence
		newer.FenceRevision++
		if _, err := p.env.Control(t.Context(), execenv.ControlRequest{Fence: newer, State: execenv.ControlRunning}); err != nil {
			t.Fatalf("Control() error = %v", err)
		}
		if _, err := p.env.ObserveProcesses(t.Context(), p.fence, 0, 1); !errors.Is(err, execenv.ErrConflict) {
			t.Fatalf("ObserveProcesses(older revision) error = %v, want ErrConflict", err)
		}
		if _, err := p.env.ObserveProcesses(t.Context(), newer, 0, 1); err != nil {
			t.Fatalf("ObserveProcesses(newer revision) error = %v", err)
		}
	})

	t.Run("long command lines are truncated", func(t *testing.T) {
		p := startTraced(t, factory)
		base := p.settle()
		many := []string{"/bin/true"}
		for i := range execenv.MaxProcessArgs + 6 {
			many = append(many, fmt.Sprintf("many%d", i))
		}
		wide := []string{"/bin/true"}
		for i := range 12 {
			wide = append(wide, fmt.Sprintf("wide%02d", i)+strings.Repeat("w", 94))
		}
		p.run(many)
		p.run(wide)
		var gotMany, gotWide *execenv.ProcessEvent
		p.collect(base, func(events []execenv.ProcessEvent) bool {
			for i := range events {
				event := &events[i]
				if event.Kind != execenv.ProcessStarted || len(event.Argv) < 2 {
					continue
				}
				switch {
				case strings.HasPrefix(event.Argv[1], "many0"):
					gotMany = event
				case strings.HasPrefix(event.Argv[1], "wide00"):
					gotWide = event
				}
			}
			return gotMany != nil && gotWide != nil
		})
		if !gotMany.Truncated || len(gotMany.Argv) != execenv.MaxProcessArgs || !slices.Equal(gotMany.Argv, many[:execenv.MaxProcessArgs]) {
			t.Fatalf("argument count not bounded: %d args, truncated %v", len(gotMany.Argv), gotMany.Truncated)
		}
		total := 0
		for i, arg := range gotWide.Argv {
			total += len(arg)
			if !strings.HasPrefix(wide[i], arg) || i < len(gotWide.Argv)-1 && arg != wide[i] {
				t.Fatalf("argument %d is not a prefix: %q", i, arg)
			}
		}
		if !gotWide.Truncated || total > execenv.MaxProcessArgvBytes || total < execenv.MaxProcessArgvBytes-100 {
			t.Fatalf("argument bytes not bounded: %d bytes, truncated %v", total, gotWide.Truncated)
		}
	})

	t.Run("overflow is counted, never silent", func(t *testing.T) {
		p := startTraced(t, factory)
		base := p.settle()
		burst := execenv.MaxProcessEvents/2 + 4
		for i := range burst {
			p.run([]string{"/bin/true", fmt.Sprintf("burst%d", i)})
		}
		// Read the full log without acknowledging, so the burst overflows.
		cursor, seen := base, 0
		deadline := time.Now().Add(60 * time.Second)
		for seen < execenv.MaxProcessEvents {
			page := p.page(cursor)
			for _, event := range page.Events {
				if event.Kind == execenv.ProcessDropped {
					t.Fatalf("loss reported before space returned: %+v", event)
				}
			}
			seen += len(page.Events)
			cursor = page.Next
			if len(page.Events) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("log never filled: %d events", seen)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		if err := p.env.AcknowledgeProcesses(t.Context(), p.fence, cursor); err != nil {
			t.Fatalf("AcknowledgeProcesses() error = %v", err)
		}
		end := []string{"/bin/true", "end"}
		p.run(end)
		var lost uint64
		markers := 0
		events := p.collect(cursor, func(events []execenv.ProcessEvent) bool {
			_, ok := pairs(events, [][]string{end})
			return ok
		})
		for _, event := range events {
			if event.Kind == execenv.ProcessDropped {
				lost += event.Dropped
				markers++
			}
		}
		recorded := uint64(seen + len(events) - markers)
		if markers != 1 || lost == 0 || recorded+lost != uint64(2*burst+2) {
			t.Fatalf("%d markers counting %d lost, %d recorded; want one marker and %d events in all", markers, lost, recorded, 2*burst+2)
		}
	})

	t.Run("revoke ends the log", func(t *testing.T) {
		p := startTraced(t, factory)
		p.settle()
		if err := p.env.Revoke(t.Context()); err != nil {
			t.Fatalf("Revoke() error = %v", err)
		}
		if _, err := p.env.ObserveProcesses(t.Context(), p.fence, 0, 1); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("ObserveProcesses(revoked) error = %v, want ErrRevoked", err)
		}
		if err := p.env.AcknowledgeProcesses(t.Context(), p.fence, 0); !errors.Is(err, execenv.ErrRevoked) {
			t.Fatalf("AcknowledgeProcesses(revoked) error = %v, want ErrRevoked", err)
		}
	})
}

type traced struct {
	t        *testing.T
	env      execenv.Env
	fence    execenv.ProjectionFence
	workload Workload
}

func startTraced(t *testing.T, factory TracedFactory) *traced {
	t.Helper()
	got := factory(t)
	return &traced{t: t, env: got.Env, fence: got.Fence, workload: got.Workload}
}

func (p *traced) run(argv []string) {
	p.t.Helper()
	if err := p.workload.Run(p.t.Context(), argv); err != nil {
		p.t.Fatalf("Run(%v) error = %v", argv, err)
	}
}

// page reads one page and checks its sequencing.
func (p *traced) page(after uint64) execenv.ProcessPage {
	p.t.Helper()
	page, err := p.env.ObserveProcesses(p.t.Context(), p.fence, after, execenv.MaxProcessPage)
	if err != nil {
		p.t.Fatalf("ObserveProcesses(%d) error = %v", after, err)
	}
	for i, event := range page.Events {
		if event.Sequence != after+uint64(i)+1 {
			p.t.Fatalf("event %d has sequence %d after %d", i, event.Sequence, after)
		}
		if err := execenv.ValidateProcessEvent(event); err != nil {
			p.t.Fatalf("invalid event %+v", event)
		}
	}
	if want := after + uint64(len(page.Events)); page.Next != want {
		p.t.Fatalf("Next = %d, want %d", page.Next, want)
	}
	return page
}

// collect reads events after a position until done accepts them.
func (p *traced) collect(after uint64, done func([]execenv.ProcessEvent) bool) []execenv.ProcessEvent {
	p.t.Helper()
	var events []execenv.ProcessEvent
	deadline := time.Now().Add(30 * time.Second)
	for {
		page := p.page(after)
		events = append(events, page.Events...)
		after = page.Next
		if done(events) {
			return events
		}
		if len(page.Events) == 0 {
			if time.Now().After(deadline) {
				p.t.Fatalf("process log incomplete: %+v", events)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// settle runs one command, waits for its exit, and acknowledges everything
// so far. It returns the acknowledged position.
func (p *traced) settle() uint64 {
	p.t.Helper()
	settle := []string{"/bin/true", "settle"}
	// The guest knows the argv of a process it launches even if it exits
	// before the process log can read its metadata.
	if err := p.workload.Start(p.t.Context(), "settle", settle); err != nil {
		p.t.Fatalf("start settle command: %v", err)
	}
	var through uint64
	p.collect(0, func(events []execenv.ProcessEvent) bool {
		found, ok := pairs(events, [][]string{settle})
		if ok {
			through = events[found[0][1]].Sequence
		}
		return ok
	})
	if err := p.env.AcknowledgeProcesses(p.t.Context(), p.fence, through); err != nil {
		p.t.Fatalf("AcknowledgeProcesses() error = %v", err)
	}
	return through
}

// pairs finds, for each command in order, its start and the exit of the
// same PID that follows it before that PID starts again.
func pairs(events []execenv.ProcessEvent, commands [][]string) ([][2]int, bool) {
	found := make([][2]int, 0, len(commands))
	for _, argv := range commands {
		start := slices.IndexFunc(events, func(e execenv.ProcessEvent) bool {
			return e.Kind == execenv.ProcessStarted && slices.Equal(e.Argv, argv)
		})
		if start < 0 {
			return nil, false
		}
		exit := -1
		for i := start + 1; i < len(events); i++ {
			if events[i].PID != events[start].PID {
				continue
			}
			if events[i].Kind == execenv.ProcessExited {
				exit = i
			}
			break
		}
		if exit < 0 {
			return nil, false
		}
		found = append(found, [2]int{start, exit})
	}
	return found, true
}
