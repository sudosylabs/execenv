// Package tree computes the next projected snapshot for ReplaceTree and Apply.
//
// Grant callers never import this package. Memory and the guest agent are
// the adapters: they commit the snapshot to a map or to Home.
package tree

import (
	"maps"

	"github.com/sudosylabs/execenv"
)

// Node is one path in a snapshot. Directories have nil Data.
type Node struct {
	Kind    execenv.NodeKind
	Version execenv.Version
	Data    []byte
}

// Snapshot is the current projection keyed by validated relative path.
type Snapshot map[string]Node

// Replace returns a snapshot that matches tree exactly. Nil Data on a
// file means keep the body already stored at that Path and Version.
func Replace(current Snapshot, tree execenv.Tree) (Snapshot, error) {
	if len(tree) > execenv.MaxTreeEntries {
		return nil, execenv.ErrTooLarge
	}
	var total int64
	seen := make(map[string]struct{}, len(tree))
	next := make(Snapshot, len(tree))
	for _, item := range tree {
		if err := execenv.ValidatePath(item.Path); err != nil {
			return nil, err
		}
		if _, dup := seen[item.Path]; dup {
			return nil, execenv.ErrInvalid
		}
		seen[item.Path] = struct{}{}
		switch item.Kind {
		case execenv.KindDirectory:
			if item.Version != "" || item.Data != nil {
				return nil, execenv.ErrInvalid
			}
			next[item.Path] = Node{Kind: execenv.KindDirectory}
		case execenv.KindFile:
			body, err := resolveBody(current[item.Path], item)
			if err != nil {
				return nil, err
			}
			total += int64(len(body))
			if int64(len(body)) > execenv.MaxFileBytes || total > execenv.MaxTreeBytes {
				return nil, execenv.ErrTooLarge
			}
			next[item.Path] = Node{Kind: execenv.KindFile, Version: item.Version, Data: body}
		default:
			return nil, execenv.ErrInvalid
		}
	}
	return next, nil
}

func resolveBody(existing Node, item execenv.Node) ([]byte, error) {
	if item.Data != nil {
		return append([]byte(nil), item.Data...), nil
	}
	if existing.Kind != execenv.KindFile || existing.Version == "" || existing.Version != item.Version {
		return nil, execenv.ErrInvalid
	}
	return append([]byte(nil), existing.Data...), nil
}

// Apply returns a new snapshot with batch applied, or an error and the
// original left unused. Callers must not commit on error.
func Apply(current Snapshot, batch execenv.Batch) (Snapshot, error) {
	if len(batch.Mutations) > execenv.MaxTreeEntries {
		return nil, execenv.ErrTooLarge
	}
	next := maps.Clone(current)
	if next == nil {
		next = make(Snapshot)
	}
	var total int64
	for _, mut := range batch.Mutations {
		if err := applyOne(next, mut, &total); err != nil {
			return nil, err
		}
	}
	return next, nil
}

func applyOne(next Snapshot, mut execenv.Mutation, total *int64) error {
	if err := execenv.ValidatePath(mut.Path); err != nil {
		return err
	}
	switch mut.Op {
	case execenv.OpCreate:
		if _, ok := next[mut.Path]; ok {
			return execenv.ErrConflict
		}
		return put(next, mut, total)
	case execenv.OpReplace:
		ent, ok := next[mut.Path]
		if !ok || ent.Kind != execenv.KindFile {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.Version != mut.Expected {
			return execenv.ErrConflict
		}
		return put(next, mut, total)
	case execenv.OpDelete:
		ent, ok := next[mut.Path]
		if !ok {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.Kind == execenv.KindFile && ent.Version != mut.Expected {
			return execenv.ErrConflict
		}
		delete(next, mut.Path)
		return nil
	case execenv.OpMove:
		if err := execenv.ValidatePath(mut.From); err != nil {
			return err
		}
		ent, ok := next[mut.From]
		if !ok {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.Kind == execenv.KindFile && ent.Version != mut.Expected {
			return execenv.ErrConflict
		}
		if _, exists := next[mut.Path]; exists {
			return execenv.ErrConflict
		}
		delete(next, mut.From)
		next[mut.Path] = ent
		return nil
	default:
		return execenv.ErrInvalid
	}
}

func put(next Snapshot, mut execenv.Mutation, total *int64) error {
	switch mut.Kind {
	case execenv.KindDirectory:
		if mut.Data != nil || mut.Version != "" {
			return execenv.ErrInvalid
		}
		next[mut.Path] = Node{Kind: execenv.KindDirectory}
		return nil
	case execenv.KindFile:
		if mut.Data == nil {
			return execenv.ErrInvalid
		}
		if int64(len(mut.Data)) > execenv.MaxFileBytes {
			return execenv.ErrTooLarge
		}
		*total += int64(len(mut.Data))
		if *total > execenv.MaxTreeBytes {
			return execenv.ErrTooLarge
		}
		next[mut.Path] = Node{
			Kind:    execenv.KindFile,
			Version: mut.Version,
			Data:    append([]byte(nil), mut.Data...),
		}
		return nil
	default:
		return execenv.ErrInvalid
	}
}
