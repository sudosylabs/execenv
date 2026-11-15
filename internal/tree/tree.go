// Package tree computes the next physical snapshot for a journal projection.
//
// Grant callers never import this package. Memory and the guest agent are
// the adapters: they commit the snapshot to a map or to Home.
package tree

import (
	"maps"
	"path"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
)

// NodeKind is a file or directory in a projected tree.
type NodeKind uint8

const (
	// KindFile is a regular file.
	KindFile NodeKind = iota
	// KindDirectory is a directory. The path itself is its only state.
	KindDirectory
)

// Item is one path of a complete physical tree. Content names a file's
// body in the content store; directories have none and no Version.
type Item struct {
	Path    string
	Kind    NodeKind
	Version execenv.Version
	Content content.Blob
}

// Tree is a complete physical snapshot: listed paths exist, unlisted paths
// do not.
type Tree []Item

// Op is one physical filesystem change.
type Op uint8

const (
	// OpCreate adds a path that did not exist.
	OpCreate Op = iota + 1
	// OpReplace changes the body of an existing file.
	OpReplace
	// OpMove renames a path. From is the old path, Path is the new one.
	OpMove
	// OpDelete removes a path.
	OpDelete
)

// Mutation is one physical change in a Batch. Expected is an optimistic
// fence for files; empty applies unconditionally.
type Mutation struct {
	// Recursive is required to delete a nonempty directory.
	Recursive bool
	// Overwrite replaces whatever the workspace holds at Path; a directory
	// create then never adopts a directory already there.
	Overwrite bool
	Op        Op
	Path      string
	From      string
	Kind      NodeKind
	Version   execenv.Version
	Expected  execenv.Version
	Content   content.Blob
}

// Batch is an atomic group of mutations: applied completely or not at all.
type Batch struct {
	Mutations []Mutation
}

// Node is one path in a snapshot.
type Node struct {
	Kind    NodeKind
	Version execenv.Version
	Content content.Blob
}

// Snapshot is the current projection keyed by validated relative path.
type Snapshot map[string]Node

// FromTree returns a snapshot that matches tree exactly. Every file carries
// its body; nil Data on a file is ErrInvalid.
func FromTree(tree Tree) (Snapshot, error) {
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
		case KindDirectory:
			if item.Version != "" || item.Content != (content.Blob{}) {
				return nil, execenv.ErrInvalid
			}
			next[item.Path] = Node{Kind: KindDirectory}
		case KindFile:
			if item.Content.Digest == "" {
				return nil, execenv.ErrInvalid
			}
			next[item.Path] = Node{Kind: KindFile, Version: item.Version, Content: item.Content}
		default:
			return nil, execenv.ErrInvalid
		}
	}
	if err := validateTopology(next, true); err != nil {
		return nil, err
	}
	return next, nil
}

// Apply returns a new snapshot with batch applied, or an error and the
// original left unused. Callers must not commit on error.
func Apply(current Snapshot, batch Batch) (Snapshot, error) {
	return apply(current, batch, true)
}

func apply(current Snapshot, batch Batch, bounded bool) (Snapshot, error) {
	next := maps.Clone(current)
	if next == nil {
		next = make(Snapshot)
	}
	for _, mut := range batch.Mutations {
		if err := applyOne(next, mut); err != nil {
			return nil, err
		}
	}
	if err := validateTopology(next, bounded); err != nil {
		return nil, err
	}
	return next, nil
}

func applyOne(next Snapshot, mut Mutation) error {
	if err := execenv.ValidatePath(mut.Path); err != nil {
		return err
	}
	if mut.Recursive && mut.Op != OpDelete {
		return execenv.ErrInvalid
	}
	switch mut.Op {
	case OpCreate:
		if _, ok := next[mut.Path]; ok {
			return execenv.ErrConflict
		}
		return put(next, mut)
	case OpReplace:
		ent, ok := next[mut.Path]
		if !ok || ent.Kind != KindFile {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.Version != mut.Expected {
			return execenv.ErrConflict
		}
		return put(next, mut)
	case OpDelete:
		ent, ok := next[mut.Path]
		if !ok {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.Kind == KindFile && ent.Version != mut.Expected {
			return execenv.ErrConflict
		}
		if ent.Kind == KindFile && mut.Recursive {
			return execenv.ErrInvalid
		}
		prefix := mut.Path + "/"
		for key := range next {
			if strings.HasPrefix(key, prefix) {
				if !mut.Recursive {
					return execenv.ErrConflict
				}
				delete(next, key)
			}
		}
		delete(next, mut.Path)
		return nil
	case OpMove:
		if err := execenv.ValidatePath(mut.From); err != nil {
			return err
		}
		ent, ok := next[mut.From]
		if !ok {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.Kind == KindFile && ent.Version != mut.Expected {
			return execenv.ErrConflict
		}
		if _, exists := next[mut.Path]; exists {
			return execenv.ErrConflict
		}
		if strings.HasPrefix(mut.Path, mut.From+"/") {
			return execenv.ErrInvalid
		}
		moved := make(Snapshot)
		for key, node := range next {
			if key != mut.From && !strings.HasPrefix(key, mut.From+"/") {
				continue
			}
			dest := mut.Path + strings.TrimPrefix(key, mut.From)
			if err := execenv.ValidatePath(dest); err != nil {
				return err
			}
			if _, exists := next[dest]; exists {
				return execenv.ErrConflict
			}
			moved[dest] = node
		}
		for key := range next {
			if key == mut.From || strings.HasPrefix(key, mut.From+"/") {
				delete(next, key)
			}
		}
		maps.Copy(next, moved)
		return nil
	default:
		return execenv.ErrInvalid
	}
}

func put(next Snapshot, mut Mutation) error {
	switch mut.Kind {
	case KindDirectory:
		if mut.Content != (content.Blob{}) || mut.Version != "" {
			return execenv.ErrInvalid
		}
		next[mut.Path] = Node{Kind: KindDirectory}
		return nil
	case KindFile:
		if mut.Content.Digest == "" {
			return execenv.ErrInvalid
		}
		next[mut.Path] = Node{Kind: KindFile, Version: mut.Version, Content: mut.Content}
		return nil
	default:
		return execenv.ErrInvalid
	}
}

// Missing parent entries retain the original implicit-directory convention;
// an explicitly projected file can never become another node's parent.
func validateTopology(nodes Snapshot, _ bool) error {
	for key := range nodes {
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			if ancestor, ok := nodes[parent]; ok && ancestor.Kind != KindDirectory {
				return execenv.ErrConflict
			}
		}
	}
	return nil
}
