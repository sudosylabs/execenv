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
	identityPath := func(name string) string {
		if movedTo != "" && (name == movedTo || strings.HasPrefix(name, movedTo+"/")) {
			return movedFrom + strings.TrimPrefix(name, movedTo)
		}
		return name
	}
	observed := e.observed(next, func(name string) bool {
		_, reported := e.guestTree.Nodes[identityPath(name)]
		return reported
	})
	candidate := &guesttree.Snapshot{Nodes: make(map[string]guesttree.Node, len(next))}
	for name, node := range next {
		if !observed(name) {
			continue
		}
		identity := e.guestTree.Nodes[identityPath(name)].Identity
		if identity == "" {
			identity = rand.Text()
		}
		kind := execenv.ProjectionFile
		if node.Kind == execenv.KindDirectory {
			kind = execenv.ProjectionDirectory
		}
		candidate.Nodes[name] = guesttree.Node{Identity: identity, Kind: kind, Data: bytes.Clone(node.Data)}
		// Every directory has an explicit identity. Refuse instead of inventing
		// an unobserved parent.
		if parent := path.Dir(name); parent != "." {
			if entry, ok := next[parent]; !ok || entry.Kind != execenv.KindDirectory {
				return execenv.ErrConflict
			}
		}
	}
	// The observed tree keeps the projection bound even though ignored guest
	// paths do not count toward it.
	if len(candidate.Nodes) > execenv.MaxTreeEntries {
		return execenv.ErrTooLarge
	}
	return e.recordGuestSnapshot(candidate)
}

// observed returns the ignore decision for files, the tree the guest now
// holds. reported says whether the stream already holds a path (after any
// move). Without rules every path is observed.
func (e *environment) observed(files tree.Snapshot, reported func(string) bool) func(string) bool {
	rules := e.projection.Ignore()
	if rules == nil {
		return func(string) bool { return true }
	}
	body, present := []byte(nil), false
	if node, ok := files[rules.File()]; ok && node.Kind == execenv.KindFile {
		body, present = node.Data, true
	}
	policy := rules.Policy(body, present)
	if !policy.Active() {
		return func(string) bool { return true }
	}
	dirs := make(map[string]bool, len(files))
	for name, node := range files {
		dirs[name] = node.Kind == execenv.KindDirectory
	}
	kept := policy.Keep(dirs, func(name string) bool {
		return e.projection.Holds(name) || reported(name)
	})
	return func(name string) bool {
		_, ok := kept[name]
		return ok
	}
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
		// A projected ignore-file edit changes what is observed without any
		// guest write; rescan as the agent does before every page.
		if e.projection.Ignore() != nil {
			if err := e.recordGuest(e.files, "", ""); err != nil {
				return err
			}
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
