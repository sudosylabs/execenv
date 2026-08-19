package agent

import (
	"os"
	"path/filepath"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/tree"
)

// rewriteHome makes Home match files exactly. New contents are written to
// a sibling directory first so a failed write leaves Home unchanged.
func rewriteHome(home string, files tree.Snapshot) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(home), "."+filepath.Base(home)+"-next-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := writeNodes(staging, files); err != nil {
		return err
	}
	if err := clearChildren(home); err != nil {
		return err
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if err := os.Rename(filepath.Join(staging, ent.Name()), filepath.Join(home, ent.Name())); err != nil {
			return err
		}
	}
	return nil
}

func clearChildren(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if err := os.RemoveAll(filepath.Join(home, ent.Name())); err != nil {
			return err
		}
	}
	return nil
}

func writeNodes(home string, files tree.Snapshot) error {
	for path, ent := range files {
		full, err := execenv.ResolvePath(home, path)
		if err != nil {
			return err
		}
		if ent.Kind == execenv.KindDirectory {
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, ent.Data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// applyHome applies batch to disk. Guest files the batch does not name stay.
// On failure this batch is reversed. On success stash files are discarded.
func applyHome(home string, batch execenv.Batch) error {
	var undos []func() error
	var drop []string
	rollback := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			_ = undos[i]()
		}
	}
	for _, mut := range batch.Mutations {
		undo, stashPath, err := applyDisk(home, mut)
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

func applyDisk(home string, mut execenv.Mutation) (undo func() error, stashPath string, err error) {
	full, err := execenv.ResolvePath(home, mut.Path)
	if err != nil {
		return nil, "", err
	}
	nop := func() error { return nil }
	switch mut.Op {
	case execenv.OpCreate, execenv.OpReplace:
		prev, side, err := stash(full)
		if err != nil {
			return nil, "", err
		}
		if mut.Kind == execenv.KindDirectory {
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
		if err := os.WriteFile(full, mut.Data, 0o644); err != nil {
			_ = prev()
			return nil, "", err
		}
		return prev, side, nil
	case execenv.OpDelete:
		prev, side, err := stash(full)
		if err != nil {
			return nil, "", err
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			_ = prev()
			return nil, "", err
		}
		return prev, side, nil
	case execenv.OpMove:
		from, err := execenv.ResolvePath(home, mut.From)
		if err != nil {
			return nil, "", err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, "", err
		}
		if err := os.Rename(from, full); err != nil {
			return nil, "", err
		}
		return func() error { return os.Rename(full, from) }, "", nil
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
