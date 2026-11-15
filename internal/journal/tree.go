package journal

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/tree"
)

func applyHome(store *content.Store, home string, batch tree.Batch) error {
	var undos []func() error
	var drop []string
	rollback := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			_ = undos[i]()
		}
	}
	for _, mut := range batch.Mutations {
		undo, stashPath, err := applyDisk(store, home, mut)
		if err != nil {
			rollback()
			return err
		}
		undos = append(undos, undo)
		if stashPath != "" {
			drop = append(drop, stashPath)
		}
	}
	for _, path := range drop {
		_ = os.RemoveAll(path)
	}
	return nil
}

func applyDisk(store *content.Store, home string, mut tree.Mutation) (undo func() error, stashPath string, err error) {
	full, err := execenv.ResolvePath(home, mut.Path)
	if err != nil {
		return nil, "", err
	}
	nop := func() error { return nil }
	switch mut.Op {
	case tree.OpCreate, tree.OpReplace:
		// The snapshot proves nothing observed is here, so anything present
		// is an ignored guest path. A projected directory adopts an ignored
		// directory; a file replaces what was there, as a checkout does.
		if info, err := os.Lstat(full); err == nil && info.IsDir() && mut.Kind == tree.KindDirectory {
			return nop, "", nil
		}
		prev, side, err := stash(full)
		if err != nil {
			return nil, "", err
		}
		if mut.Kind == tree.KindDirectory {
			if err := os.MkdirAll(full, 0o755); err != nil {
				_ = prev()
				return nil, "", err
			}
			return prev, side, nil
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			_ = prev()
			return nil, "", err
		}
		if err := store.Place(mut.Content, full, 0o644); err != nil {
			_ = prev()
			return nil, "", err
		}
		return prev, side, nil
	case tree.OpDelete:
		prev, side, err := stash(full)
		if err != nil {
			return nil, "", err
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			_ = prev()
			return nil, "", err
		}
		return prev, side, nil
	case tree.OpMove:
		from, err := execenv.ResolvePath(home, mut.From)
		if err != nil {
			return nil, "", err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, "", err
		}
		// An ignored guest path at the destination is replaced.
		prev, side, err := stash(full)
		if err != nil {
			return nil, "", err
		}
		if err := os.Rename(from, full); err != nil {
			_ = prev()
			return nil, "", err
		}
		return func() error {
			if err := os.Rename(full, from); err != nil {
				return err
			}
			return prev()
		}, side, nil
	default:
		return nop, "", execenv.ErrInvalid
	}
}

// stash moves path aside if it exists. side is the stash path to drop on
// success. undo restores path. A missing path undoes with RemoveAll.
func stash(path string) (undo func() error, side string, err error) {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return func() error { return os.RemoveAll(path) }, "", nil
	} else if err != nil {
		return nil, "", err
	}
	dir := filepath.Dir(path)
	tmp, err := os.MkdirTemp(dir, "."+filepath.Base(path)+"-")
	if err != nil {
		return nil, "", err
	}
	if err := os.Remove(tmp); err != nil {
		return nil, "", err
	}
	if err := os.Rename(path, tmp); err != nil {
		return nil, "", err
	}
	return func() error {
		_ = os.RemoveAll(path)
		return os.Rename(tmp, path)
	}, tmp, nil
}

// reconcileHome makes home match next, starting from current, what home
// holds now: paths next does not name are removed, directories made, and
// files placed unless current already holds the same body. A seeded
// workspace therefore writes only what differs from its seed.
func reconcileHome(store *content.Store, home string, current *guesttree.Snapshot, next tree.Snapshot) error {
	names := make([]string, 0, len(current.Nodes))
	for name := range current.Nodes {
		names = append(names, name)
	}
	slices.Sort(names)
	removed := ""
	for _, name := range names {
		if removed != "" && strings.HasPrefix(name, removed+"/") {
			continue
		}
		have := current.Nodes[name]
		want, ok := next[name]
		if ok && (want.Kind == tree.KindDirectory) == (have.Kind == execenv.ProjectionDirectory) {
			continue
		}
		full, err := execenv.ResolvePath(home, name)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(full); err != nil {
			return err
		}
		removed = name
	}
	wanted := make([]string, 0, len(next))
	for name := range next {
		wanted = append(wanted, name)
	}
	slices.Sort(wanted)
	for _, name := range wanted {
		node := next[name]
		full, err := execenv.ResolvePath(home, name)
		if err != nil {
			return err
		}
		if node.Kind == tree.KindDirectory {
			if err := os.MkdirAll(full, 0o755); err != nil {
				return content.DiskError(err)
			}
			continue
		}
		if have, ok := current.Nodes[name]; ok && have.Kind == execenv.ProjectionFile && have.Content == node.Content {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return content.DiskError(err)
		}
		if err := store.Place(node.Content, full, 0o644); err != nil {
			return err
		}
	}
	return nil
}
