// Package watchlog keeps a bounded replay window for guest-originated events.
package watchlog

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sudosylabs/execenv"
)

var fallbackEpoch atomic.Uint64

const DefaultCapacity = 64

// Log is not concurrency-safe. Its owner serializes access with the
// environment state that decides whether an event is guest-originated.
type Log struct {
	epoch    string
	next     uint64
	capacity int
	events   []execenv.Event
}

func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Log{epoch: newEpoch(), capacity: capacity}
}

// Reset starts a new stream generation. Cursors from the old projection can
// no longer be resumed.
func (l *Log) Reset() {
	l.epoch = newEpoch()
	l.next = 0
	l.events = nil
}

func (l *Log) Append(event execenv.Event) execenv.Event {
	l.next++
	event.Cursor = makeCursor(l.epoch, l.next)
	l.events = append(l.events, event)
	if len(l.events) > l.capacity {
		copy(l.events, l.events[len(l.events)-l.capacity:])
		l.events = l.events[:l.capacity]
	}
	return event
}

func (l *Log) Current() execenv.Cursor {
	return makeCursor(l.epoch, l.next)
}

// Since returns the observation's starting cursor and events strictly after
// cursor. Empty starts at the current position without replay.
func (l *Log) Since(cursor execenv.Cursor) (execenv.Cursor, []execenv.Event, error) {
	current := l.Current()
	if cursor == "" {
		return current, nil, nil
	}
	if len(cursor) > execenv.MaxCursorBytes {
		return "", nil, execenv.ErrLagged
	}
	if cursor == current {
		return cursor, nil, nil
	}
	epoch, seq, ok := parseCursor(cursor)
	if !ok || epoch != l.epoch || seq > l.next {
		return "", nil, execenv.ErrLagged
	}
	oldest := l.next - uint64(len(l.events))
	if seq < oldest {
		return "", nil, execenv.ErrLagged
	}
	start := int(seq - oldest)
	out := append([]execenv.Event(nil), l.events[start:]...)
	return cursor, out, nil
}

func makeCursor(epoch string, seq uint64) execenv.Cursor {
	return execenv.Cursor(epoch + ":" + strconv.FormatUint(seq, 10))
}

func parseCursor(cursor execenv.Cursor) (string, uint64, bool) {
	epoch, raw, ok := strings.Cut(string(cursor), ":")
	if !ok || epoch == "" || raw == "" {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	return epoch, seq, err == nil
}

func newEpoch() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	// Randomness failure does not make execution unsafe. The process-local
	// fallback still prevents a cursor from being interpreted as a sequence.
	return "fallback-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(fallbackEpoch.Add(1), 10)
}
