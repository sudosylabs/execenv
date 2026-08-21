package isolated

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/sudosylabs/execenv"
)

func (e *environment) ReplaceTree(ctx context.Context, tree execenv.Tree) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("replace", err)
	}
	client, err := e.live("replace")
	if err != nil {
		return err
	}
	return client.ReplaceTree(ctx, tree)
}

func (e *environment) Apply(ctx context.Context, batch execenv.Batch) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("apply", err)
	}
	client, err := e.live("apply")
	if err != nil {
		return err
	}
	return client.Apply(ctx, batch)
}

func (e *environment) Watch(ctx context.Context, after execenv.Cursor) (execenv.Observation, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("watch", err)
	}
	client, err := e.live("watch")
	if err != nil {
		return nil, err
	}
	return client.Watch(ctx, after)
}

func (e *environment) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("open", err)
	}
	client, err := e.live("open")
	if err != nil {
		return nil, err
	}
	return client.Open(ctx, path)
}

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
	return env.writeGuest(path, data)
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
	return env.removeGuest(path)
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
	return env.moveGuest(from, to)
}

// GuestWriter writes the host-visible workspace. The agent watches that
// same directory, so Watch and Open see the change without a fake PTY.
func (e *environment) writeGuest(path string, data []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("guest"); err != nil {
		return err
	}
	full, err := execenv.ResolvePath(e.treeDir, path)
	if err != nil {
		return execenv.Error("guest", err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return execenv.Error("guest", err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return execenv.Error("guest", err)
	}
	return nil
}

func (e *environment) removeGuest(path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("guest"); err != nil {
		return err
	}
	full, err := execenv.ResolvePath(e.treeDir, path)
	if err != nil {
		return execenv.Error("guest", err)
	}
	if _, err := os.Lstat(full); err != nil {
		return execenv.Error("guest", execenv.ErrNotFound)
	}
	if err := os.RemoveAll(full); err != nil {
		return execenv.Error("guest", err)
	}
	return nil
}

func (e *environment) moveGuest(from, to string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("guest"); err != nil {
		return err
	}
	src, err := execenv.ResolvePath(e.treeDir, from)
	if err != nil {
		return execenv.Error("guest", err)
	}
	dst, err := execenv.ResolvePath(e.treeDir, to)
	if err != nil {
		return execenv.Error("guest", err)
	}
	if _, err := os.Lstat(src); err != nil {
		return execenv.Error("guest", execenv.ErrNotFound)
	}
	if _, err := os.Lstat(dst); err == nil {
		return execenv.Error("guest", execenv.ErrConflict)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return execenv.Error("guest", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return execenv.Error("guest", err)
	}
	return nil
}
