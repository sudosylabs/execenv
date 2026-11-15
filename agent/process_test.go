package agent

import (
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/sudosylabs/execenv"
)

const (
	testAgent = 50
	testGroup = "/execenv-workload"
)

// fakeProc is the /proc of a simulated guest. A missing entry is a process
// that has already exited.
type fakeProc map[int]fakeTask

type fakeTask struct {
	cgroup  string
	parent  int
	cmdline string
	cwd     string
}

func (p fakeProc) task(pid int) (fakeTask, error) {
	task, ok := p[pid]
	if !ok {
		return fakeTask{}, os.ErrNotExist
	}
	return task, nil
}
func (p fakeProc) cgroup(pid int) (string, error) { t, err := p.task(pid); return t.cgroup, err }
func (p fakeProc) parent(pid int) (int, error)    { t, err := p.task(pid); return t.parent, err }
func (p fakeProc) cmdline(pid int) ([]byte, error) {
	t, err := p.task(pid)
	return []byte(t.cmdline), err
}
func (p fakeProc) cwd(pid int) (string, error) { t, err := p.task(pid); return t.cwd, err }

func fork(parent, child int) kernelEvent {
	return kernelEvent{Kind: kernelFork, Parent: parent, PID: child, TGID: child}
}
func execOf(pid int) kernelEvent { return kernelEvent{Kind: kernelExec, PID: pid, TGID: pid} }
func exitOf(pid int, status uint32) kernelEvent {
	return kernelEvent{Kind: kernelExit, PID: pid, TGID: pid, Status: status}
}

// run feeds events, letting each step change the simulated /proc first.
func run(t *testing.T, tr *tracker, proc fakeProc, steps ...func(fakeProc) kernelEvent) []execenv.ProcessEvent {
	t.Helper()
	var out []execenv.ProcessEvent
	for _, step := range steps {
		if event, ok := tr.handle(step(proc)); ok {
			out = append(out, event)
		}
	}
	return out
}

func set(pid int, task fakeTask, event kernelEvent) func(fakeProc) kernelEvent {
	return func(p fakeProc) kernelEvent { p[pid] = task; return event }
}
func gone(pid int, event kernelEvent) func(fakeProc) kernelEvent {
	return func(p fakeProc) kernelEvent { delete(p, pid); return event }
}
func same(event kernelEvent) func(fakeProc) kernelEvent {
	return func(fakeProc) kernelEvent { return event }
}

func summary(events []execenv.ProcessEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		switch e.Kind {
		case execenv.ProcessStarted:
			line := "start"
			if e.Unread {
				line += " unread"
			}
			for _, arg := range e.Argv {
				line += " " + arg
			}
			out = append(out, line)
		case execenv.ProcessExited:
			if e.ExitCode != nil {
				out = append(out, "exit "+string(rune('0'+*e.ExitCode)))
			} else {
				out = append(out, "signal "+string(rune('0'+e.Signal)))
			}
		case execenv.ProcessDropped:
			out = append(out, "dropped")
		}
	}
	return out
}

func newTestTracker(proc fakeProc) *tracker {
	return newTracker(proc, testGroup, testAgent, "/workspace", "/run/execenv/home")
}

// The helper's child first runs the workload bootstrap, which replaces
// itself with the shell: only the shell is recorded. Its commands are, and
// a fork that never starts a program is part of its parent.
func TestTrackerRecordsTheShellAndItsCommands(t *testing.T) {
	proc := fakeProc{}
	tr := newTestTracker(proc)
	tr.launched = func(pid int) (launch, bool) {
		if pid == 10 {
			return launch{tag: "shell", argv: []string{"/bin/sh", "-l"}, dir: "/workspace"}, true
		}
		return launch{tag: "never"}, true
	}
	shell := fakeTask{cgroup: testGroup, parent: testAgent, cmdline: "/usr/local/libexec/execenv-guest\x00workload-exec\x00/bin/sh\x00-l\x00", cwd: "/workspace"}
	events := run(t, tr, proc,
		set(10, shell, fork(testAgent, 10)),
		same(execOf(10)),
		set(10, fakeTask{cgroup: testGroup, parent: testAgent, cmdline: "/bin/sh\x00-l\x00", cwd: "/workspace"}, execOf(10)),
		set(11, fakeTask{cgroup: testGroup, parent: 10, cwd: "/workspace"}, fork(10, 11)),
		set(11, fakeTask{cgroup: testGroup, parent: 10, cmdline: "ls\x00-la\x00", cwd: "/workspace/src"}, execOf(11)),
		gone(11, exitOf(11, 2<<8)),
		set(12, fakeTask{cgroup: testGroup, parent: 10}, fork(10, 12)),
		gone(12, exitOf(12, 0)),
		gone(10, exitOf(10, 1)),
	)
	if got, want := summary(events), []string{"start /bin/sh -l", "start ls -la", "exit 2", "signal 1"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if events[0].ParentPID != testAgent || events[1].ParentPID != 10 || events[1].Dir != "/workspace/src" || events[1].PID != 11 {
		t.Fatalf("details: %+v", events)
	}
	// The process Start created carries its tag; what it starts does not.
	if events[0].Tag != "shell" || events[1].Tag != "" {
		t.Fatalf("tags = %q, %q; want shell and none", events[0].Tag, events[1].Tag)
	}
}

// Processes outside the workload hierarchy, the helper and init among them,
// are never recorded; threads are not processes.
func TestTrackerFiltersByWorkloadHierarchy(t *testing.T) {
	proc := fakeProc{}
	tr := newTestTracker(proc)
	events := run(t, tr, proc,
		set(20, fakeTask{cgroup: "/", parent: 1, cmdline: "mkfs\x00"}, fork(1, 20)),
		same(execOf(20)),
		gone(20, exitOf(20, 0)),
		set(21, fakeTask{cgroup: "/execenv-workload-other", parent: 1, cmdline: "x\x00"}, execOf(21)),
		set(30, fakeTask{cgroup: testGroup + "/nested", parent: 1, cmdline: "y\x00", cwd: "/etc"}, execOf(30)),
		same(kernelEvent{Kind: kernelExit, PID: 31, TGID: 30}),
		gone(30, exitOf(30, 0)),
	)
	if got, want := summary(events), []string{"start y", "exit 0"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if events[0].Dir != "" {
		t.Fatalf("directory outside the workspace reported: %q", events[0].Dir)
	}
}

// A process that ends before it is read is recorded as unread, and a
// descendant is known by lineage even when its /proc entry is gone.
func TestTrackerRecordsUnreadProcesses(t *testing.T) {
	proc := fakeProc{}
	tr := newTestTracker(proc)
	events := run(t, tr, proc,
		set(40, fakeTask{cgroup: testGroup, parent: 1, cmdline: "sh\x00"}, execOf(40)),
		same(fork(40, 41)),
		same(execOf(41)),
		same(exitOf(41, 0)),
		set(42, fakeTask{cgroup: testGroup, parent: 40}, fork(40, 42)),
		same(execOf(42)),
	)
	if got, want := summary(events), []string{"start sh", "start unread", "exit 0", "start unread"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if events[1].ParentPID != 40 || events[1].Argv != nil || events[1].Dir != "" {
		t.Fatalf("unread details: %+v", events[1])
	}
}

// A process Start created that ends before it is read is still reported
// with the command the helper ran and its tag; another unread one is not.
func TestTrackerFillsAnUnreadStartedProcess(t *testing.T) {
	proc := fakeProc{}
	tr := newTestTracker(proc)
	tr.launched = func(pid int) (launch, bool) {
		if pid == 50 {
			return launch{tag: "job", argv: []string{"/bin/true", "job"}, dir: "/workspace/src"}, true
		}
		return launch{}, false
	}
	events := run(t, tr, proc,
		set(50, fakeTask{cgroup: testGroup, parent: testAgent, cmdline: "/usr/local/libexec/execenv-guest\x00workload-exec\x00/bin/true\x00job\x00"}, fork(testAgent, 50)),
		same(execOf(50)),
		gone(50, execOf(50)),
		same(exitOf(50, 0)),
	)
	if got, want := summary(events), []string{"start /bin/true job", "exit 0"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if e := events[0]; e.Unread || e.Tag != "job" || e.Dir != "/workspace/src" {
		t.Fatalf("filled start = %+v", e)
	}
}

// A reused PID pairs by order: each exit follows its own start.
func TestTrackerOrdersReusedPIDs(t *testing.T) {
	proc := fakeProc{}
	tr := newTestTracker(proc)
	events := run(t, tr, proc,
		set(60, fakeTask{cgroup: testGroup, parent: 1, cmdline: "first\x00"}, execOf(60)),
		gone(60, exitOf(60, 0)),
		same(exitOf(60, 0)),
		set(60, fakeTask{cgroup: testGroup, parent: 1, cmdline: "second\x00"}, fork(1, 60)),
		same(execOf(60)),
		set(60, fakeTask{cgroup: testGroup, parent: 1, cmdline: "third\x00"}, execOf(60)),
		gone(60, exitOf(60, 3<<8)),
		same(kernelEvent{Kind: kernelLost}),
	)
	if got, want := summary(events), []string{"start first", "exit 0", "start second", "start third", "exit 3", "dropped"}; !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	for _, e := range events[:5] {
		if e.PID != 60 {
			t.Fatalf("pid: %+v", e)
		}
	}
}

type sliceReader struct{ events []kernelEvent }

func (r *sliceReader) next() (kernelEvent, error) {
	if len(r.events) == 0 {
		return kernelEvent{}, errors.New("closed")
	}
	event := r.events[0]
	r.events = r.events[1:]
	return event, nil
}
func (r *sliceReader) close() error { return nil }

func TestKernelProcessesSkipsUnrecordedEvents(t *testing.T) {
	proc := fakeProc{70: {cgroup: testGroup, parent: 1, cmdline: "run\x00"}}
	source := &kernelProcesses{reader: &sliceReader{events: []kernelEvent{fork(1, 70), execOf(70), {Kind: kernelExit, PID: 71, TGID: 70}}}, tracker: newTestTracker(proc)}
	event, err := source.Next()
	if err != nil || event.Kind != execenv.ProcessStarted || event.Argv[0] != "run" {
		t.Fatalf("first: %+v %v", event, err)
	}
	if _, err := source.Next(); err == nil {
		t.Fatal("thread exit recorded")
	}
}
