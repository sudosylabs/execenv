package agent

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/mux"
	"github.com/sudosylabs/execenv/internal/processes"
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

const (
	methodProcessEvents = "process_events"
	// processQueueEvents bounds the events the helper holds while the host
	// link is slow. Beyond it events are counted, never waited for.
	processQueueEvents = execenv.MaxProcessEvents
	processBatchEvents = 128
)

// pumpProcesses sends every event to the host as soon as it is taken. The
// source is read on its own goroutine into a bounded queue, so a slow host
// link costs counted events, never a stalled workload.
func (s *server) pumpProcesses(ctx context.Context, source ProcessSource) {
	queue := newProcessQueue()
	s.processes.Store(true)
	go func() {
		<-ctx.Done()
		_ = source.Close()
		queue.close()
	}()
	go func() {
		defer s.processes.Store(false)
		for {
			event, err := source.Next()
			if err != nil {
				queue.close()
				return
			}
			queue.push(boundProcess(event))
		}
	}()
	go func() {
		for {
			batch, ok := queue.take()
			if !ok {
				return
			}
			extra, err := encodeExtra(batch)
			if err != nil {
				continue
			}
			if err := s.sess.Send(frame{Kind: mux.KindEvent, Method: methodProcessEvents, Extra: extra}); err != nil {
				return
			}
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

type processQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	events []execenv.ProcessEvent
	lost   uint64
	closed bool
}

func newProcessQueue() *processQueue {
	q := &processQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *processQueue) push(event execenv.ProcessEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	defer q.cond.Signal()
	if event.Kind == execenv.ProcessDropped {
		q.lost += event.Dropped
		return
	}
	if q.lost > 0 {
		if len(q.events)+2 > processQueueEvents {
			q.lost++
			return
		}
		q.events = append(q.events, dropped(q.lost))
		q.lost = 0
	}
	if len(q.events) >= processQueueEvents {
		q.lost++
		return
	}
	q.events = append(q.events, event)
}

func (q *processQueue) take() ([]execenv.ProcessEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.events) == 0 && q.lost == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return nil, false
	}
	if len(q.events) == 0 {
		lost := q.lost
		q.lost = 0
		return []execenv.ProcessEvent{dropped(lost)}, true
	}
	n := min(len(q.events), processBatchEvents)
	batch := append([]execenv.ProcessEvent(nil), q.events[:n]...)
	q.events = append(q.events[:0], q.events[n:]...)
	return batch, true
}

func (q *processQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

func dropped(count uint64) execenv.ProcessEvent {
	return execenv.ProcessEvent{Kind: execenv.ProcessDropped, Dropped: count, At: time.Now()}
}

// deliverProcesses retains one batch from the guest. A batch that does not
// decode counts as one lost event.
func (c *Client) deliverProcesses(f frame) {
	if f.Method != methodProcessEvents {
		return
	}
	var batch []execenv.ProcessEvent
	if decodeExtra(f.Extra, &batch) != nil || len(batch) == 0 {
		c.processes.Lost(1)
		return
	}
	for _, event := range batch {
		c.processes.Append(event)
	}
}

// ObserveProcesses pages the process log the host retains for this guest.
// The log outlives the guest link, so a lost link loses no retained event.
func (c *Client) ObserveProcesses(ctx context.Context, after uint64, limit int) (execenv.ProcessPage, error) {
	if err := ctx.Err(); err != nil {
		return execenv.ProcessPage{}, err
	}
	return c.processes.Page(after, limit)
}

// AcknowledgeProcesses releases retained process events through a sequence.
func (c *Client) AcknowledgeProcesses(ctx context.Context, through uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.processes.Acknowledge(through)
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
type tracker struct {
	proc   procInfo
	group  string
	agent  int
	roots  []string
	member map[int]*member
}

type member struct {
	parent int
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
	return &tracker{proc: proc, group: group, agent: agent, roots: clean, member: make(map[int]*member)}
}

func (t *tracker) inWorkload(pid int) bool {
	path, err := t.proc.cgroup(pid)
	return err == nil && (path == t.group || strings.HasPrefix(path, t.group+"/"))
}

// admit returns the tracked workload process tgid, adding one that the
// lineage missed but the hierarchy proves.
func (t *tracker) admit(tgid, parent int) *member {
	if m := t.member[tgid]; m != nil {
		return m
	}
	if !t.inWorkload(tgid) {
		return nil
	}
	if parent == 0 {
		parent, _ = t.proc.parent(tgid)
	}
	m := &member{parent: parent, launch: parent == t.agent}
	t.member[tgid] = m
	return m
}

func (t *tracker) handle(event kernelEvent) (execenv.ProcessEvent, bool) {
	switch event.Kind {
	case kernelLost:
		return execenv.ProcessEvent{Kind: execenv.ProcessDropped, Dropped: 1, At: event.At}, true
	case kernelFork:
		// A new thread is not a new process.
		if event.PID != event.TGID {
			return execenv.ProcessEvent{}, false
		}
		delete(t.member, event.TGID)
		if t.member[event.Parent] != nil {
			t.member[event.TGID] = &member{parent: event.Parent}
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
		delete(t.member, event.TGID)
		if m == nil || !m.started {
			return execenv.ProcessEvent{}, false
		}
		return processes.Exited(event.TGID, event.Status, event.At), true
	}
	return execenv.ProcessEvent{}, false
}

func (t *tracker) started(pid, parent int, at time.Time) execenv.ProcessEvent {
	event := execenv.ProcessEvent{Kind: execenv.ProcessStarted, PID: pid, ParentPID: parent, At: at}
	raw, err := t.proc.cmdline(pid)
	// A zombie has an empty command line: it ended before it was read.
	if err != nil || len(raw) == 0 {
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
}

func (k *kernelProcesses) Next() (execenv.ProcessEvent, error) {
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
