package memory

import (
	"context"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/tree"
)

// ReadGuest returns the body the environment currently sees at path. It is
// a test hook beside the guest writers below; production callers read guest
// changes only through Observe.
func (h *Host) ReadGuest(ctx context.Context, id execenv.ID, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(path); err != nil {
		return nil, execenv.Error("guest", err)
	}
	env, err := h.grant(id)
	if err != nil {
		return nil, err
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	node, ok := env.files[path]
	if !ok || node.Kind != execenv.KindFile {
		return nil, execenv.Error("guest", execenv.ErrNotFound)
	}
	return append([]byte(nil), node.Data...), nil
}

// WriteGuest, RemoveGuest and MoveGuest mutate the projection as if the
// environment did it. They are how tests of this adapter produce semantic
// observations without a real guest. Production callers never need them.
func (h *Host) WriteGuest(ctx context.Context, id execenv.ID, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(path); err != nil {
		return execenv.Error("guest", err)
	}
	if int64(len(data)) > execenv.MaxFileBytes {
		return execenv.Error("guest", execenv.ErrTooLarge)
	}
	env, err := h.grant(id)
	if err != nil {
		return err
	}
	return env.commitGuest(func() (execenv.Mutation, error) {
		op := execenv.OpCreate
		if _, existed := env.files[path]; existed {
			op = execenv.OpReplace
		}
		return execenv.Mutation{Op: op, Path: path, Kind: execenv.KindFile, Version: "guest", Data: append([]byte{}, data...)}, nil
	}, "", "")
}

func (h *Host) RemoveGuest(ctx context.Context, id execenv.ID, path string) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(path); err != nil {
		return execenv.Error("guest", err)
	}
	env, err := h.grant(id)
	if err != nil {
		return err
	}
	return env.commitGuest(func() (execenv.Mutation, error) {
		node, ok := env.files[path]
		if !ok {
			return execenv.Mutation{}, execenv.ErrNotFound
		}
		return execenv.Mutation{Op: execenv.OpDelete, Path: path, Recursive: node.Kind == execenv.KindDirectory}, nil
	}, "", "")
}

// MkdirGuest creates one directory as the environment would. Its parent
// must already exist.
func (h *Host) MkdirGuest(ctx context.Context, id execenv.ID, path string) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(path); err != nil {
		return execenv.Error("guest", err)
	}
	env, err := h.grant(id)
	if err != nil {
		return err
	}
	return env.commitGuest(func() (execenv.Mutation, error) {
		if _, exists := env.files[path]; exists {
			return execenv.Mutation{}, execenv.ErrConflict
		}
		return execenv.Mutation{Op: execenv.OpCreate, Path: path, Kind: execenv.KindDirectory}, nil
	}, "", "")
}

func (h *Host) MoveGuest(ctx context.Context, id execenv.ID, from, to string) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(from); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(to); err != nil {
		return execenv.Error("guest", err)
	}
	env, err := h.grant(id)
	if err != nil {
		return err
	}
	return env.commitGuest(func() (execenv.Mutation, error) {
		if _, ok := env.files[from]; !ok {
			return execenv.Mutation{}, execenv.ErrNotFound
		}
		if _, exists := env.files[to]; exists {
			return execenv.Mutation{}, execenv.ErrConflict
		}
		return execenv.Mutation{Op: execenv.OpMove, From: from, Path: to}, nil
	}, from, to)
}

func (h *Host) grant(id execenv.ID) (*environment, error) {
	h.mu.Lock()
	env, ok := h.grants[id]
	h.mu.Unlock()
	if !ok {
		return nil, execenv.Error("guest", execenv.ErrRevoked)
	}
	return env, nil
}

// commitGuest records the observation before installing the guest tree, so a
// refused capture leaves both the files and the observation log unchanged.
func (e *environment) commitGuest(plan func() (execenv.Mutation, error), movedFrom, movedTo string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("guest"); err != nil {
		return err
	}
	mutation, err := plan()
	if err != nil {
		return execenv.Error("guest", err)
	}
	apply := tree.Apply
	if e.projection.Ignore() != nil {
		// Ignored guest paths never count toward the observed tree's bound;
		// recordGuest enforces that bound on what it observes.
		apply = tree.ApplyGuest
	}
	next, err := apply(e.files, execenv.Batch{Mutations: []execenv.Mutation{mutation}})
	if err != nil {
		return err
	}
	if e.projection.Initialized() {
		if err := e.recordGuest(next, movedFrom, movedTo); err != nil {
			return err
		}
	}
	e.files = next
	return nil
}

func (e *environment) guard(op string) error {
	if e.lost {
		return execenv.Error(op, execenv.ErrLost)
	}
	if e.revoked {
		return execenv.Error(op, execenv.ErrRevoked)
	}
	if e.frozen {
		return execenv.Error(op, execenv.ErrFrozen)
	}
	return nil
}
