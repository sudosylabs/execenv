package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"path"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/observations"
	"github.com/sudosylabs/execenv/internal/tree"
)

var _ execenv.SemanticEnv = (*environment)(nil)

func (e *environment) refreshGuestProjection(mutations []execenv.ProjectionMutation) {
	if e.guestTree == nil {
		e.guestTree = &guesttree.Snapshot{Nodes: make(map[string]guesttree.Node)}
	}
	affected := func(name string) bool {
		if mutations == nil {
			return true
		}
		for _, m := range mutations {
			for _, root := range []string{m.Path, m.DestinationPath} {
				if root != "" && (name == root || strings.HasPrefix(name, root+"/")) {
					return true
				}
			}
		}
		return false
	}
	for name := range e.guestTree.Nodes {
		if affected(name) {
			delete(e.guestTree.Nodes, name)
		}
	}
	for name, node := range e.projection.Nodes() {
		if affected(name) {
			e.guestTree.Nodes[name] = guesttree.Node{Identity: node.Identity, Kind: node.Kind, Data: node.Data}
		}
	}
}

// recordGuest commits the immutable observation before installing a guest tree.
// Memory provides the same serialization barrier for simulated guest writes.
func (e *environment) recordGuest(next tree.Snapshot, movedFrom, movedTo string) error {
	if e.semantic == nil || e.guestTree == nil {
		return execenv.ErrUnavailable
	}
	candidate := &guesttree.Snapshot{Nodes: make(map[string]guesttree.Node, len(next))}
	for name, node := range next {
		identityPath := name
		if movedTo != "" && (name == movedTo || strings.HasPrefix(name, movedTo+"/")) {
			identityPath = movedFrom + strings.TrimPrefix(name, movedTo)
		}
		identity := e.guestTree.Nodes[identityPath].Identity
		if identity == "" {
			identity = rand.Text()
		}
		kind := execenv.ProjectionFile
		if node.Kind == execenv.KindDirectory {
			kind = execenv.ProjectionDirectory
		}
		candidate.Nodes[name] = guesttree.Node{Identity: identity, Kind: kind, Data: bytes.Clone(node.Data)}
		// The strong protocol requires explicit directory identity, unlike legacy
		// implicit parent entries. Refuse instead of inventing an unobserved parent.
		if parent := path.Dir(name); parent != "." {
			if entry, ok := next[parent]; !ok || entry.Kind != execenv.KindDirectory {
				return execenv.ErrConflict
			}
		}
	}
	return e.recordGuestSnapshot(candidate)
}
func (e *environment) recordGuestSnapshot(candidate *guesttree.Snapshot) error {
	changes, err := guesttree.Diff(e.guestTree, candidate)
	if err != nil {
		return err
	}
	captured := make([]observations.Captured, 0, len(changes))
	versions := make(map[string]execenv.Version)
	for _, node := range e.projection.Nodes() {
		versions[node.Identity] = node.Version
	}
	for _, change := range changes {
		event := execenv.ExecutionObservation{Fence: e.projectionFence, BasedOnWorkspaceCursor: e.projection.Cursor(), Operation: execenv.ObservationOperation(change.Operation), NodeIdentity: change.Identity, Kind: change.Kind, Path: change.Path, DestinationPath: change.Destination, ExpectedContentVersion: versions[change.Identity]}
		if change.Operation == guesttree.CreateFile || change.Operation == guesttree.CreateDirectory {
			event.ExpectedContentVersion = ""
		}
		if change.Operation == guesttree.Delete && change.Kind == execenv.ProjectionDirectory {
			recursive := change.Recursive
			event.Recursive = &recursive
		}
		var body []byte
		if change.Operation == guesttree.CreateFile || change.Operation == guesttree.WriteFile {
			body = change.Data
		}
		captured = append(captured, observations.Captured{Event: event, Data: body})
	}
	if err := e.semantic.Append(captured); err != nil {
		return err
	}
	for range changes {
		if err := e.projection.GuestChanged(); err != nil {
			return err
		}
	}
	e.guestTree = candidate
	return nil
}

func (e *environment) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	var page execenv.ObservationPage
	err := e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.semantic == nil {
			return execenv.ErrConflict
		}
		var err error
		page, err = e.semantic.Page(after, limit)
		return err
	})
	return page, err
}
func (e *environment) OpenObservationContent(ctx context.Context, fence execenv.ProjectionFence, reference execenv.ContentReference) (io.ReadCloser, error) {
	var body []byte
	err := e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.semantic == nil {
			return execenv.ErrConflict
		}
		var err error
		body, err = e.semantic.Content(reference)
		return err
	})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}
func (e *environment) AcknowledgeObservations(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	return e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.semantic == nil {
			return execenv.ErrConflict
		}
		return e.semantic.Acknowledge(through)
	})
}

func (e *environment) ConfirmObservationProjection(ctx context.Context, binding execenv.ObservationProjection) error {
	return e.control.Run(ctx, binding.Fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.semantic == nil {
			return execenv.ErrConflict
		}
		return e.projection.RegisterObservation(binding, e.semantic.Event)
	})
}
