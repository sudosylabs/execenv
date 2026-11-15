// Package observations retains immutable, ordered guest effects until their
// caller durably records an outcome. The owning environment serializes access.
package observations

import (
	"crypto/rand"
	"encoding/json"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
)

const maxRetainedEvents = 4096

// Captured is one guest effect. A created or written file names its body,
// already in the content store.
type Captured struct {
	Event   execenv.ExecutionObservation
	Content content.Blob
}
type retained struct {
	event execenv.ExecutionObservation
	blob  content.Blob
}

// Log holds events and the names of their bodies; the bodies are on disk.
type Log struct {
	grant        execenv.ID
	epoch        string
	cursor       uint64
	acknowledged uint64
	events       []retained
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
	for i, item := range captured {
		event := item.Event
		if event.Fence.ExecutionGrantID != l.grant || event.Fence.EnvironmentEpoch != l.epoch || event.HostSequence != 0 || event.Content != nil {
			return execenv.ErrInvalid
		}
		event.HostSequence = l.cursor + uint64(i) + 1
		body := event.Operation == execenv.ObservationCreateFile || event.Operation == execenv.ObservationWriteFile
		if body {
			if item.Content.Digest == "" {
				return execenv.ErrInvalid
			}
			event.Content = &execenv.ContentReference{Size: item.Content.Size, Digest: item.Content.Digest, TransferID: rand.Text()}
		} else if item.Content != (content.Blob{}) {
			return execenv.ErrInvalid
		}
		if execenv.ValidateExecutionObservation(event) != nil {
			return execenv.ErrInvalid
		}
		next = append(next, retained{event: clone(event), blob: item.Content})
	}
	l.events = append(l.events, next...)
	l.cursor += uint64(len(next))
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

// Content is the body a retained event names. An acknowledged or unknown
// reference is ErrConflict.
func (l *Log) Content(reference execenv.ContentReference) (content.Blob, error) {
	if execenv.ValidateContentReference(reference) != nil {
		return content.Blob{}, execenv.ErrInvalid
	}
	for _, item := range l.events {
		if item.event.Content != nil && *item.event.Content == reference {
			return item.blob, nil
		}
	}
	return content.Blob{}, execenv.ErrConflict
}

// Live adds the digest of every retained body to live.
func (l *Log) Live(live map[string]bool) {
	for _, item := range l.events {
		if item.blob.Digest != "" {
			live[item.blob.Digest] = true
		}
	}
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

func (l *Log) Event(sequence uint64) (execenv.ExecutionObservation, error) {
	for _, item := range l.events {
		if item.event.HostSequence == sequence {
			return clone(item.event), nil
		}
	}
	return execenv.ExecutionObservation{}, execenv.ErrLagged
}
