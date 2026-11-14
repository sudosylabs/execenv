// Package processes retains one epoch's process log on the host until its
// caller acknowledges it, and bounds the command lines guests report.
package processes

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sudosylabs/execenv"
)

// Log is one epoch's retained process log. It is safe for concurrent use:
// producers append while callers page and acknowledge.
//
// Appending never blocks and never evicts an unacknowledged event. While the
// log is full, new events are counted instead, and one ProcessDropped event
// carrying the count takes the next sequence once space returns.
type Log struct {
	mu           sync.Mutex
	events       []retained
	bytes        int
	produced     uint64
	acknowledged uint64
	lost         uint64
	now          func() time.Time
	maxEvents    int
	maxBytes     int
}

type retained struct {
	event execenv.ProcessEvent
	size  int
}

// New returns an empty log with the documented retention bounds.
func New() *Log {
	return &Log{now: time.Now, maxEvents: execenv.MaxProcessEvents, maxBytes: execenv.MaxProcessBytes}
}

// Append sequences and retains event. Its Sequence must be zero. A dropped
// event adds its count to the loss. An event that does not validate is
// counted as lost rather than retained, so nothing disappears silently.
func (l *Log) Append(event execenv.ProcessEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if event.Kind == execenv.ProcessDropped && event.Sequence == 0 && event.Dropped > 0 && execenv.ValidateProcessEvent(withSequence(event, 1)) == nil {
		l.lose(event.Dropped)
		l.flush()
		return
	}
	l.flush()
	if event.Sequence != 0 || l.produced >= execenv.MaxProjectionCursor {
		l.lose(1)
		return
	}
	event = clone(event)
	event.Sequence = l.produced + 1
	if execenv.ValidateProcessEvent(event) != nil {
		l.lose(1)
		return
	}
	size := encodedSize(event)
	if l.lost > 0 || !l.fits(size) {
		l.lose(1)
		return
	}
	l.push(event, size)
}

// Lost counts events a producer lost before it could append them.
func (l *Log) Lost(count uint64) {
	if count == 0 {
		return
	}
	l.Append(execenv.ProcessEvent{Kind: execenv.ProcessDropped, Dropped: count, At: l.now()})
}

func (l *Log) lose(count uint64) {
	if l.lost > ^uint64(0)-count {
		l.lost = ^uint64(0)
		return
	}
	l.lost += count
}

// flush turns the counted loss into one dropped event once it fits.
func (l *Log) flush() {
	if l.lost == 0 || l.produced >= execenv.MaxProjectionCursor {
		return
	}
	event := execenv.ProcessEvent{Sequence: l.produced + 1, Kind: execenv.ProcessDropped, Dropped: l.lost, At: l.now()}
	size := encodedSize(event)
	if !l.fits(size) {
		return
	}
	l.lost = 0
	l.push(event, size)
}

func (l *Log) fits(size int) bool {
	return len(l.events) < l.maxEvents && l.bytes+size <= l.maxBytes
}

func (l *Log) push(event execenv.ProcessEvent, size int) {
	l.events = append(l.events, retained{event: event, size: size})
	l.bytes += size
	l.produced = event.Sequence
}

// Page returns up to limit events after the given sequence.
func (l *Log) Page(after uint64, limit int) (execenv.ProcessPage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if after > l.produced || limit < 1 || limit > execenv.MaxProcessPage {
		return execenv.ProcessPage{}, execenv.ErrInvalid
	}
	if after < l.acknowledged {
		return execenv.ProcessPage{}, execenv.ErrLagged
	}
	page := execenv.ProcessPage{Events: make([]execenv.ProcessEvent, 0, min(limit, len(l.events))), Next: after}
	size := len(`{"events":[],"next":}`) + 20
	for _, item := range l.events[after-l.acknowledged:] {
		if len(page.Events) == limit || size+item.size+1 > execenv.MaxProjectionMetadataBytes {
			break
		}
		page.Events = append(page.Events, clone(item.event))
		page.Next = item.event.Sequence
		size += item.size + 1
	}
	return page, nil
}

// Acknowledge releases the events through a sequence.
func (l *Log) Acknowledge(through uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if through > l.produced {
		return execenv.ErrInvalid
	}
	if through <= l.acknowledged {
		return nil
	}
	count := int(through - l.acknowledged)
	for i := range count {
		l.bytes -= l.events[i].size
		l.events[i] = retained{}
	}
	l.events = l.events[count:]
	l.acknowledged = through
	l.flush()
	return nil
}

func withSequence(event execenv.ProcessEvent, sequence uint64) execenv.ProcessEvent {
	event.Sequence = sequence
	return event
}

func clone(event execenv.ProcessEvent) execenv.ProcessEvent {
	if event.Argv != nil {
		event.Argv = append([]string(nil), event.Argv...)
	}
	if event.ExitCode != nil {
		code := *event.ExitCode
		event.ExitCode = &code
	}
	return event
}

func encodedSize(event execenv.ProcessEvent) int {
	raw, err := json.Marshal(event)
	if err != nil {
		return execenv.MaxProcessBytes + 1
	}
	return len(raw)
}

// Argv bounds a command line to MaxProcessArgs arguments and
// MaxProcessArgvBytes bytes, replacing invalid UTF-8. It reports whether
// anything was cut.
func Argv(args []string) ([]string, bool) {
	out := make([]string, 0, min(len(args), execenv.MaxProcessArgs))
	total := 0
	for i, arg := range args {
		if i == execenv.MaxProcessArgs {
			return out, true
		}
		arg = strings.ToValidUTF8(arg, "�")
		if room := execenv.MaxProcessArgvBytes - total; len(arg) > room {
			cut := room
			for cut > 0 && !utf8.RuneStart(arg[cut]) {
				cut--
			}
			if cut > 0 {
				out = append(out, arg[:cut])
			}
			return out, true
		}
		out = append(out, arg)
		total += len(arg)
	}
	return out, false
}

// Exited builds the exit event for a raw wait status as the kernel reports
// it: an exit code in bits 8-15, or a terminating signal in bits 0-6.
func Exited(pid int, status uint32, at time.Time) execenv.ProcessEvent {
	event := execenv.ProcessEvent{Kind: execenv.ProcessExited, PID: pid, At: at}
	if signal := int(status & 0x7f); signal != 0 {
		event.Signal = signal
	} else {
		code := int(status>>8) & 0xff
		event.ExitCode = &code
	}
	return event
}
