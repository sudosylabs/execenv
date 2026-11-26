// Package observations retains ordered guest effects until their caller
// durably records an outcome. A created or written file is named by its
// digest; its body stays in the workspace and is read from there. The owning
// environment serializes access.
package observations

import (
	"crypto/rand"
	"encoding/json"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
)

const maxRetainedEvents = 4096

// Captured is one guest effect. A created or written file names the digest
// and size of the body it was captured with.
type Captured struct {
	Event   execenv.ExecutionObservation
	Content content.Blob
}
type retained struct {
	event execenv.ExecutionObservation
	blob  content.Blob
}

// Log holds events and the digests of their bodies.
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
func (l *Log) IsLagged() bool                 { return l.lagged }

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
	raw, _ := json.Marshal(page)
	size := len(raw)
	for _, item := range l.events[after-l.acknowledged:] {
		raw, err := json.Marshal(item.event)
		if err != nil {
			return execenv.ObservationPage{}, execenv.ErrInvalid
		}
		size += len(raw)
		if len(page.Observations) != 0 {
			size++ // comma between array elements
		}
		if size > execenv.MaxProjectionMetadataBytes {
			break
		}
		page.Observations = append(page.Observations, clone(item.event))
		if len(page.Observations) == limit {
			break
		}
	}
	return page, nil
}

// Content is the path and body a retained event names. An acknowledged or
// unknown reference is ErrConflict.
func (l *Log) Content(reference execenv.ContentReference) (string, content.Blob, error) {
	if execenv.ValidateContentReference(reference) != nil {
		return "", content.Blob{}, execenv.ErrInvalid
	}
	for _, item := range l.events {
		if item.event.Content != nil && *item.event.Content == reference {
			if item.event.DiscardedBy != 0 {
				// An overwrite replaced the body.
				return "", content.Blob{}, execenv.ErrStale
			}
			return item.event.Path, item.blob, nil
		}
	}
	return "", content.Blob{}, execenv.ErrConflict
}

// Replaced is a subtree an overwriting projection replaced: the journal
// cursor of the mutation and the path at its root.
type Replaced struct {
	JournalCursor uint64
	Path          string
}

// Discard is one retained observation an overwrite discards.
type Discard struct {
	Sequence uint64
	By       uint64
}

func inside(name string, roots []Replaced) (uint64, bool) {
	var by uint64
	found := false
	for _, root := range roots {
		if name == root.Path || strings.HasPrefix(name, root.Path+"/") {
			by, found = max(by, root.JournalCursor), true
		}
	}
	return by, found
}

// Discardable lists, in order, the retained observations not yet discarded
// whose every path lies in one of roots, each with the latest mutation
// that replaced one of them. An observation that names a path inside and a
// path outside is ErrConflict: an overwrite cannot discard half a move.
func (l *Log) Discardable(roots []Replaced) ([]Discard, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	var out []Discard
	for _, item := range l.events {
		event := item.event
		if event.DiscardedBy != 0 {
			continue
		}
		by, in := inside(event.Path, roots)
		if event.DestinationPath != "" {
			to, toIn := inside(event.DestinationPath, roots)
			if in != toIn {
				return nil, execenv.ErrConflict
			}
			by = max(by, to)
		}
		if in {
			out = append(out, Discard{Sequence: event.HostSequence, By: by})
		}
	}
	return out, nil
}

// Discard marks the observations Discardable listed.
func (l *Log) Discard(discards []Discard) {
	by := make(map[uint64]uint64, len(discards))
	for _, d := range discards {
		by[d.Sequence] = d.By
	}
	for i := range l.events {
		if cursor, ok := by[l.events[i].event.HostSequence]; ok {
			l.events[i].event.DiscardedBy = cursor
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
