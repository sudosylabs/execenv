package agent

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/processes"
	"github.com/sudosylabs/execenv/internal/wire"
)

// ProcessSource reports the processes a workload starts and how they end.
// A contained workload uses the guest kernel's process events; only tests
// supply their own.
type ProcessSource interface {
	// Next blocks for the next started, exited or dropped event, with a
	// zero Sequence. It fails once the source is closed or broken.
	Next() (execenv.ProcessEvent, error)
	Close() error
}

// recordProcesses appends every event to the guest's process log as it is
// taken. The log is bounded: past its bounds it counts what it drops, and
// the workload is never held back.
func (s *server) recordProcesses(ctx context.Context, source ProcessSource) {
	s.processes.Store(true)
	go func() {
		<-ctx.Done()
		_ = source.Close()
	}()
	go func() {
		defer s.processes.Store(false)
		for {
			event, err := source.Next()
			if err != nil {
				return
			}
			if event.Kind == execenv.ProcessDropped {
				s.log.Lost(event.Dropped)
				continue
			}
			s.log.Append(boundProcess(event))
		}
	}()
}

// boundProcess applies the documented command line bounds to any source.
func boundProcess(event execenv.ProcessEvent) execenv.ProcessEvent {
	if event.Kind != execenv.ProcessStarted {
		return event
	}
	argv, cut := processes.Argv(event.Argv)
	if len(argv) == 0 {
		argv = nil
	}
	event.Argv, event.Truncated = argv, event.Truncated || cut
	if len(event.Dir) > execenv.MaxProcessDirBytes {
		event.Dir = ""
	}
	return event
}

// ObserveProcesses pages the process log the guest retains. It outlives the
// guest link and the host, so neither losing the link nor restarting the
// host loses a retained event.
func (c *Client) ObserveProcesses(ctx context.Context, after uint64, limit int) (execenv.ProcessPage, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.ObserveProcesses(ctx, connect.NewRequest(&execenvv1.ObserveProcessesRequest{After: after, Limit: int32(limit)}))
	if err != nil {
		return execenv.ProcessPage{}, c.failure("process_observe", err)
	}
	return wire.ProcessPageFrom(resp.Msg), nil
}

// AcknowledgeProcesses releases retained process events through a sequence.
func (c *Client) AcknowledgeProcesses(ctx context.Context, through uint64) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
	_, err := c.agent.AcknowledgeProcesses(ctx, connect.NewRequest(&execenvv1.AcknowledgeProcessesRequest{Through: through}))
	return c.failure("process_acknowledge", err)
}

// kernelKind is one raw kernel process event.
type kernelKind uint8

const (
	kernelFork kernelKind = iota + 1
	kernelExec
	kernelExit
	// kernelLost means the kernel dropped events it could not deliver.
	kernelLost
)

// kernelEvent is one decoded kernel process event. For a fork, PID and
// TGID name the child and Parent the parent's thread group. For an exit,
// Status is the raw wait status.
type kernelEvent struct {
	Kind   kernelKind
	PID    int
	TGID   int
	Parent int
	Status uint32
	At     time.Time
}

// kernelReader yields raw kernel process events in delivery order.
type kernelReader interface {
	next() (kernelEvent, error)
	close() error
}

// procInfo reads one live process. A process that has exited fails.
type procInfo interface {
	// cgroup is the process's path in the unified hierarchy.
	cgroup(pid int) (string, error)
	parent(pid int) (int, error)
	cmdline(pid int) ([]byte, error)
	cwd(pid int) (string, error)
}

// tracker turns raw kernel events into the workload's process log. It
// records program starts, not forks, and only for the workload hierarchy.
// It also knows, for every workload process, the tag of the process Start
// created that it descends from, for as long as it runs and for a while
// after it ends, so a change it made can name it.
type tracker struct {
	// mu guards member and gone: events arrive on one goroutine, tag
	// lookups on any.
	mu     sync.Mutex
	proc   procInfo
	group  string
	agent  int
	roots  []string
	member map[int]*member
	// launched finds a process Start created, by its pid. Only the helper's
	// own children are looked up.
	launched func(pid int) (launch, bool)
	// gone keeps the tags of the most recently ended processes, oldest
	// first in ended, for changes read after their writer ended.
	gone  map[int]string
	ended []int
}

// keptEnded is how many ended processes' tags the tracker keeps.
const keptEnded = 4096

// launch is what the helper knows of a process Start created: its tag,
// and the command and directory it ran it with.
type launch struct {
	tag  string
	argv []string
	dir  string
}

type member struct {
	parent int
	// tag is the tag of the process Start created that this one descends
	// from, once known.
	tag string
	// launch marks the helper's own child: its first program is the
	// helper's workload bootstrap, which replaces itself with the shell.
	launch  bool
	started bool
}

func newTracker(proc procInfo, group string, agent int, roots ...string) *tracker {
	clean := make([]string, 0, len(roots))
	for _, root := range roots {
		if root != "" {
			clean = append(clean, filepath.Clean(root))
		}
	}
	return &tracker{proc: proc, group: group, agent: agent, roots: clean, member: make(map[int]*member), gone: make(map[int]string)}
}

// tagOf is the tag of the process Start created that pid descends from, ""
// when it descends from none or the tracker does not know it.
func (t *tracker) tagOf(pid int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tagLocked(pid)
}

func (t *tracker) tagLocked(pid int) string {
	// A lineage deeper than this is not walked.
	for range 256 {
		m := t.member[pid]
		if m == nil {
			return t.gone[pid]
		}
		if m.tag != "" {
			return m.tag
		}
		if m.parent == t.agent {
			if t.launched != nil {
				if known, ok := t.launched(pid); ok {
					m.tag = known.tag
				}
			}
			return m.tag
		}
		pid = m.parent
	}
	return ""
}

// end forgets a running process, keeping its tag among the ended.
func (t *tracker) end(tgid int) {
	if t.member[tgid] == nil {
		return
	}
	tag := t.tagLocked(tgid)
	delete(t.member, tgid)
	if tag == "" {
		return
	}
	if _, kept := t.gone[tgid]; !kept {
		t.ended = append(t.ended, tgid)
	}
	t.gone[tgid] = tag
	for len(t.ended) > keptEnded {
		delete(t.gone, t.ended[0])
		t.ended = t.ended[1:]
	}
}

func (t *tracker) inWorkload(pid int) bool {
	path, err := t.proc.cgroup(pid)
	return err == nil && (path == t.group || strings.HasPrefix(path, t.group+"/"))
}

// admit returns the tracked workload process tgid, adding one proven by
// a direct launch or the workload hierarchy.
func (t *tracker) admit(tgid, parent int) *member {
	if m := t.member[tgid]; m != nil {
		return m
	}
	// The helper starts these children in the workload cgroup. A fork
	// identifying one can establish membership after /proc is gone.
	var known launch
	var launched bool
	if parent == t.agent && t.launched != nil {
		known, launched = t.launched(tgid)
	}
	if !launched && !t.inWorkload(tgid) {
		return nil
	}
	if parent == 0 {
		parent, _ = t.proc.parent(tgid)
	}
	m := &member{parent: parent, tag: known.tag, launch: parent == t.agent}
	t.member[tgid] = m
	return m
}

func (t *tracker) handle(event kernelEvent) (execenv.ProcessEvent, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch event.Kind {
	case kernelLost:
		return execenv.ProcessEvent{Kind: execenv.ProcessDropped, Dropped: 1, At: event.At}, true
	case kernelFork:
		// A new thread is not a new process.
		if event.PID != event.TGID {
			return execenv.ProcessEvent{}, false
		}
		delete(t.member, event.TGID)
		// A reused pid is a new process: an ended one's tag is not its.
		if _, held := t.gone[event.TGID]; held {
			delete(t.gone, event.TGID)
			t.ended = slices.DeleteFunc(t.ended, func(pid int) bool { return pid == event.TGID })
		}
		if parent := t.member[event.Parent]; parent != nil {
			t.member[event.TGID] = &member{parent: event.Parent, tag: t.tagLocked(event.Parent)}
		} else {
			t.admit(event.TGID, event.Parent)
		}
		return execenv.ProcessEvent{}, false
	case kernelExec:
		m := t.admit(event.TGID, 0)
		if m == nil {
			return execenv.ProcessEvent{}, false
		}
		if m.launch {
			m.launch = false
			return execenv.ProcessEvent{}, false
		}
		m.started = true
		return t.started(event.TGID, m.parent, event.At), true
	case kernelExit:
		// Only the thread group's end ends the process.
		if event.PID != event.TGID {
			return execenv.ProcessEvent{}, false
		}
		m := t.member[event.TGID]
		t.end(event.TGID)
		if m == nil || !m.started {
			return execenv.ProcessEvent{}, false
		}
		return processes.Exited(event.TGID, event.Status, event.At), true
	}
	return execenv.ProcessEvent{}, false
}

func (t *tracker) started(pid, parent int, at time.Time) execenv.ProcessEvent {
	event := execenv.ProcessEvent{Kind: execenv.ProcessStarted, PID: pid, ParentPID: parent, At: at}
	var known launch
	var ok bool
	if parent == t.agent && t.launched != nil {
		if known, ok = t.launched(pid); ok {
			event.Tag = known.tag
			if m := t.member[pid]; m != nil {
				m.tag = known.tag
			}
		}
	}
	raw, err := t.proc.cmdline(pid)
	// A zombie has an empty command line: it ended before it was read.
	if err != nil || len(raw) == 0 {
		if ok && len(known.argv) > 0 {
			// The helper ran this command itself, so it knows it.
			event.Argv, event.Truncated = processes.Argv(known.argv)
			if t.under(known.dir) && len(known.dir) <= execenv.MaxProcessDirBytes {
				event.Dir = known.dir
			}
			return event
		}
		event.Unread = true
		return event
	}
	raw = bytes.TrimSuffix(raw, []byte{0})
	args := strings.Split(string(raw), "\x00")
	event.Argv, event.Truncated = processes.Argv(args)
	if dir, err := t.proc.cwd(pid); err == nil && t.under(dir) && len(dir) <= execenv.MaxProcessDirBytes {
		event.Dir = dir
	}
	return event
}

func (t *tracker) under(dir string) bool {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return false
	}
	for _, root := range t.roots {
		if dir == root || strings.HasPrefix(dir, root+"/") {
			return true
		}
	}
	return false
}

// kernelProcesses is the contained workload's ProcessSource.
type kernelProcesses struct {
	reader  kernelReader
	tracker *tracker
	pinned  bool
}

func (k *kernelProcesses) Next() (execenv.ProcessEvent, error) {
	if !k.pinned {
		// Next is always called from one goroutine. Its thread reads each
		// process's command line between that process's exec and its exit;
		// it runs ahead of the workload so a short command cannot end first.
		k.pinned = true
		runtime.LockOSThread()
		runAhead()
	}
	for {
		raw, err := k.reader.next()
		if err != nil {
			return execenv.ProcessEvent{}, err
		}
		if event, ok := k.tracker.handle(raw); ok {
			return event, nil
		}
	}
}

func (k *kernelProcesses) Close() error { return k.reader.close() }
