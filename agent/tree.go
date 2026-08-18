package agent

import (
	"maps"
	"os"
	"path/filepath"

	"github.com/sudosylabs/execenv"
)

type node struct {
	kind    execenv.NodeKind
	version execenv.Version
	data    []byte
}

// rewriteHome makes Home match files exactly. The directory itself is
// kept so a mounted workspace is not unmounted by RemoveAll.
func rewriteHome(home string, files map[string]node) error {
	if err := clearChildren(home); err != nil {
		return err
	}
	return writeNodes(home, files)
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

func writeNodes(home string, files map[string]node) error {
	for path, ent := range files {
		full, err := execenv.ResolvePath(home, path)
		if err != nil {
			return err
		}
		if ent.kind == execenv.KindDirectory {
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, ent.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func applyHome(home string, batch execenv.Batch) error {
	for _, mut := range batch.Mutations {
		if err := applyDisk(home, mut); err != nil {
			return err
		}
	}
	return nil
}

func applyDisk(home string, mut execenv.Mutation) error {
	full, err := execenv.ResolvePath(home, mut.Path)
	if err != nil {
		return err
	}
	switch mut.Op {
	case execenv.OpCreate, execenv.OpReplace:
		if mut.Kind == execenv.KindDirectory {
			return os.MkdirAll(full, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, mut.Data, 0o644)
	case execenv.OpDelete:
		return os.RemoveAll(full)
	case execenv.OpMove:
		from, err := execenv.ResolvePath(home, mut.From)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.Rename(from, full)
	default:
		return execenv.ErrInvalid
	}
}

func replaceInto(current map[string]node, tree execenv.Tree) (map[string]node, error) {
	if len(tree) > execenv.MaxTreeEntries {
		return nil, execenv.ErrTooLarge
	}
	var total int64
	seen := make(map[string]struct{}, len(tree))
	next := make(map[string]node, len(tree))
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
			next[item.Path] = node{kind: execenv.KindDirectory}
		case execenv.KindFile:
			body, err := resolveBody(current[item.Path], item)
			if err != nil {
				return nil, err
			}
			total += int64(len(body))
			if int64(len(body)) > execenv.MaxFileBytes || total > execenv.MaxTreeBytes {
				return nil, execenv.ErrTooLarge
			}
			next[item.Path] = node{kind: execenv.KindFile, version: item.Version, data: body}
		default:
			return nil, execenv.ErrInvalid
		}
	}
	return next, nil
}

func resolveBody(existing node, item execenv.Node) ([]byte, error) {
	if item.Data != nil {
		return append([]byte(nil), item.Data...), nil
	}
	if existing.kind != execenv.KindFile || existing.version == "" || existing.version != item.Version {
		return nil, execenv.ErrInvalid
	}
	return append([]byte(nil), existing.data...), nil
}

func applyInto(current map[string]node, batch execenv.Batch) (map[string]node, error) {
	if len(batch.Mutations) > execenv.MaxTreeEntries {
		return nil, execenv.ErrTooLarge
	}
	next := maps.Clone(current)
	if next == nil {
		next = make(map[string]node)
	}
	var total int64
	for _, mut := range batch.Mutations {
		if err := applyOne(next, mut, &total); err != nil {
			return nil, err
		}
	}
	return next, nil
}

func applyOne(next map[string]node, mut execenv.Mutation, total *int64) error {
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
		if !ok || ent.kind != execenv.KindFile {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.version != mut.Expected {
			return execenv.ErrConflict
		}
		return put(next, mut, total)
	case execenv.OpDelete:
		ent, ok := next[mut.Path]
		if !ok {
			return execenv.ErrNotFound
		}
		if mut.Expected != "" && ent.kind == execenv.KindFile && ent.version != mut.Expected {
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
		if mut.Expected != "" && ent.kind == execenv.KindFile && ent.version != mut.Expected {
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

func put(next map[string]node, mut execenv.Mutation, total *int64) error {
	switch mut.Kind {
	case execenv.KindDirectory:
		if mut.Data != nil || mut.Version != "" {
			return execenv.ErrInvalid
		}
		next[mut.Path] = node{kind: execenv.KindDirectory}
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
		next[mut.Path] = node{
			kind:    execenv.KindFile,
			version: mut.Version,
			data:    append([]byte(nil), mut.Data...),
		}
		return nil
	default:
		return execenv.ErrInvalid
	}
}
