package memory

import (
	"context"
	"io"
	"maps"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/observations"
	"github.com/sudosylabs/execenv/internal/projection"
	"github.com/sudosylabs/execenv/internal/tree"
)

func (e *environment) UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, reader io.Reader) (execenv.ContentReference, error) {
	if reader == nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return execenv.ContentReference{}, err
	}
	body, err := io.ReadAll(io.LimitReader(reader, execenv.MaxFileBytes+1))
	if err != nil {
		return execenv.ContentReference{}, err
	}
	if len(body) > execenv.MaxFileBytes {
		return execenv.ContentReference{}, execenv.ErrTooLarge
	}
	var reference execenv.ContentReference
	err = e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		var err error
		reference, err = e.projection.Put(body)
		return err
	})
	return reference, err
}

func (e *environment) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	var receipt execenv.ProjectionReceipt
	err := e.control.Run(ctx, request.Fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		var err error
		receipt, err = e.projection.Initialize(request, func(snapshot execenv.Tree) error {
			if e.terminal.Live() || len(e.files) != 0 {
				return execenv.ErrConflict
			}
			next, err := tree.FromTree(snapshot)
			if err != nil {
				return err
			}
			e.files = next
			return nil
		})
		if err == nil && e.semantic == nil {
			e.projectionFence = request.Fence
			e.semantic = observations.New(e.id, e.Epoch())
			e.refreshGuestProjection(nil)
		}
		return err
	})
	return receipt, err
}

func (e *environment) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	var receipt execenv.ProjectionReceipt
	err := e.control.Run(ctx, request.Fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		// As the agent captures before every apply, fold in what a projected
		// ignore-file edit has made visible since the last guest write.
		if e.projection.Initialized() && e.projection.Ignore() != nil {
			if err := e.recordGuest(e.files, "", ""); err != nil {
				return err
			}
		}
		var err error
		receipt, err = e.projection.Apply(request, func(plan projection.Plan) error {
			prepared, err := plan.Prepare(e.guestTree)
			if err != nil {
				return err
			}
			e.files = withIgnored(prepared.Tree, ignoredFiles(e.files, e.guestTree), prepared.Batch)
			e.guestTree = prepared.Guest
			return nil
		})
		return err
	})
	return receipt, err
}

// ignoredFiles returns the guest paths the observation tree leaves out.
func ignoredFiles(files tree.Snapshot, observed *guesttree.Snapshot) tree.Snapshot {
	hidden := make(tree.Snapshot)
	for name, node := range files {
		if _, ok := observed.Nodes[name]; !ok {
			hidden[name] = node
		}
	}
	return hidden
}

// withIgnored carries ignored guest paths through a projected batch the way
// the agent's disk does: a projected directory adopts an ignored one, a
// projected file or move destination replaces what was there, a deleted
// directory takes its ignored contents with it, and a moved one carries
// them along.
func withIgnored(projected, hidden tree.Snapshot, batch execenv.Batch) tree.Snapshot {
	under := func(name, root string) bool { return name == root || strings.HasPrefix(name, root+"/") }
	for _, m := range batch.Mutations {
		switch m.Op {
		case execenv.OpCreate, execenv.OpReplace:
			if node, ok := hidden[m.Path]; ok && node.Kind == execenv.KindDirectory && m.Kind == execenv.KindDirectory {
				delete(hidden, m.Path)
				continue
			}
			for name := range hidden {
				if under(name, m.Path) {
					delete(hidden, name)
				}
			}
		case execenv.OpDelete:
			for name := range hidden {
				if under(name, m.Path) {
					delete(hidden, name)
				}
			}
		case execenv.OpMove:
			for name := range hidden {
				if under(name, m.Path) {
					delete(hidden, name)
				}
			}
			moved := make(tree.Snapshot)
			for name, node := range hidden {
				if strings.HasPrefix(name, m.From+"/") {
					delete(hidden, name)
					moved[m.Path+strings.TrimPrefix(name, m.From)] = node
				}
			}
			maps.Copy(hidden, moved)
		}
	}
	result := maps.Clone(projected)
	for name, node := range hidden {
		if _, ok := result[name]; !ok {
			result[name] = node
		}
	}
	return result
}
