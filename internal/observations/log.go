// Package observations retains immutable, ordered guest effects until their
// caller durably records an outcome. The owning environment serializes access.
package observations

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/sudosylabs/execenv"
)

const maxRetainedEvents = 4096

type Captured struct {
	Event execenv.ExecutionObservation
	Data  []byte
}
type retained struct {
	event execenv.ExecutionObservation
	data  []byte
}
type Log struct {
	grant        execenv.ID
	epoch        string
	cursor       uint64
	acknowledged uint64
	events       []retained
	bytes        int64
	lagged       bool
}

func New(grant execenv.ID, epoch string) *Log { return &Log{grant: grant, epoch: epoch} }
func (l *Log) Cursor() uint64                 { return l.cursor }
func (l *Log) Lagged()                        { l.lagged = true }

// Append validates and copies the entire capture before publishing any sequence.
// Overflow never evicts unacknowledged content; the epoch requires recovery.
func (l *Log) Append(captured []Captured) error {
	if l.lagged {
		return execenv.ErrLagged
	}
	if len(l.events)+len(captured) > maxRetainedEvents || uint64(len(captured)) > execenv.MaxProjectionCursor-l.cursor {
		l.lagged = true
		return execenv.ErrLagged
	}
	next := make([]retained, 0, len(captured))
	size := l.bytes
	for i, item := range captured {
		event := item.Event
		if event.Fence.ExecutionGrantID != l.grant || event.Fence.EnvironmentEpoch != l.epoch || event.HostSequence != 0 || event.Content != nil {
			return execenv.ErrInvalid
		}
		event.HostSequence = l.cursor + uint64(i) + 1
		body := event.Operation == execenv.ObservationCreateFile || event.Operation == execenv.ObservationWriteFile
		if body {
			if len(item.Data) > execenv.MaxFileBytes {
				l.lagged = true
				return execenv.ErrLagged
			}
			size += int64(len(item.Data))
			if size > execenv.MaxTreeBytes {
				l.lagged = true
				return execenv.ErrLagged
			}
			sum := sha256.Sum256(item.Data)
			event.Content = &execenv.ContentReference{Size: int64(len(item.Data)), Digest: hex.EncodeToString(sum[:]), TransferID: rand.Text()}
		} else if item.Data != nil {
			return execenv.ErrInvalid
		}
		if execenv.ValidateExecutionObservation(event) != nil {
			return execenv.ErrInvalid
		}
		next = append(next, retained{event: clone(event), data: bytes.Clone(item.Data)})
	}
	l.events = append(l.events, next...)
	l.cursor += uint64(len(next))
	l.bytes = size
	return nil
}
func (l *Log) Page(after uint64, limit int) (execenv.ObservationPage, error) {
	if after > l.cursor || limit < 1 || limit > execenv.MaxProjectionMutations {
		return execenv.ObservationPage{}, execenv.ErrInvalid
	}
	if after < l.acknowledged {
		return execenv.ObservationPage{}, execenv.ErrLagged
	}
	// Even after overflow, existing immutable evidence remains available. The
	// caller sees ErrLagged after draining the retained prefix and retires it.
	if l.lagged && after == l.cursor {
		return execenv.ObservationPage{}, execenv.ErrLagged
	}
	page := execenv.ObservationPage{HostCursor: l.cursor, AcknowledgedHostCursor: l.acknowledged, Observations: make([]execenv.ExecutionObservation, 0, limit)}
	for _, item := range l.events {
		if item.event.HostSequence <= after {
			continue
		}
		page.Observations = append(page.Observations, clone(item.event))
		raw, _ := json.Marshal(page)
		if len(raw) > execenv.MaxProjectionMetadataBytes {
			page.Observations = page.Observations[:len(page.Observations)-1]
			break
		}
		if len(page.Observations) == limit {
			break
		}
	}
	return page, nil
}
func (l *Log) Content(reference execenv.ContentReference) ([]byte, error) {
	if execenv.ValidateContentReference(reference) != nil {
		return nil, execenv.ErrInvalid
	}
	for _, item := range l.events {
		if item.event.Content != nil && *item.event.Content == reference {
			return bytes.Clone(item.data), nil
		}
	}
	return nil, execenv.ErrConflict
}
func (l *Log) Acknowledge(through uint64) error {
	if through > l.cursor {
		return execenv.ErrInvalid
	}
	if through <= l.acknowledged {
		return nil
	}
	count := 0
	for count < len(l.events) && l.events[count].event.HostSequence <= through {
		l.bytes -= int64(len(l.events[count].data))
		l.events[count] = retained{}
		count++
	}
	l.events = l.events[count:]
	l.acknowledged = through
	return nil
}
func clone(event execenv.ExecutionObservation) execenv.ExecutionObservation {
	if event.Content != nil {
		content := *event.Content
		event.Content = &content
	}
	if event.Recursive != nil {
		recursive := *event.Recursive
		event.Recursive = &recursive
	}
	return event
}

// Chunk streams retained event bytes without cloning the whole body per read.
func (l *Log) Chunk(reference execenv.ContentReference, offset int64, length int) ([]byte, error) {
	if execenv.ValidateContentReference(reference) != nil || offset < 0 || offset > reference.Size || length < 1 || length > 64<<10 {
		return nil, execenv.ErrInvalid
	}
	for _, item := range l.events {
		if item.event.Content != nil && *item.event.Content == reference {
			return bytes.Clone(item.data[offset:min(offset+int64(length), int64(len(item.data)))]), nil
		}
	}
	return nil, execenv.ErrConflict
}

func (l *Log) Event(sequence uint64) (execenv.ExecutionObservation, error) {
	for _, item := range l.events {
		if item.event.HostSequence == sequence {
			return clone(item.event), nil
		}
	}
	return execenv.ExecutionObservation{}, execenv.ErrLagged
}
