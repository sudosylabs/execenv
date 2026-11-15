package memory

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/sudosylabs/execenv"
)

// The in-memory host projects into a real temporary directory with the same
// journal the guest agent runs, so a projection behaves here exactly as it
// does in a microVM. "In memory" is about isolation: nothing is contained.

// lockBarrier stops the simulated guest: every guest hook below takes the
// same lock to write.
type lockBarrier struct{ mu *sync.Mutex }

// dirty is the in-memory host's watcher: every guest write and file API
// change sets it, so an Observe with nothing new does not walk.
type dirty struct{ changed atomic.Bool }

func (d *dirty) Changed() bool { return d.changed.Swap(false) }
func (d *dirty) touch()        { d.changed.Store(true) }

func (b lockBarrier) Stopped(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fn()
}

func (e *environment) UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, reader io.Reader) (execenv.ContentReference, error) {
	if reader == nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return execenv.ContentReference{}, err
	}
	if err := e.control.Run(ctx, fence, func() error { return nil }); err != nil {
		return execenv.ContentReference{}, err
	}
	blob, err := e.journal.Receive(reader)
	if err != nil {
		return execenv.ContentReference{}, err
	}
	var ref execenv.ContentReference
	err = e.journaled(ctx, fence, func() error {
		var err error
		ref, err = e.journal.Upload(fence, blob)
		return err
	})
	return ref, err
}

func (e *environment) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	var receipt execenv.ProjectionReceipt
	err := e.journaled(ctx, request.Fence, func() error {
		if e.procs.Live() && !e.journal.Initialized() {
			return execenv.ErrConflict
		}
		var err error
		receipt, err = e.journal.Initialize(ctx, request)
		return err
	})
	return receipt, err
}

func (e *environment) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	var receipt execenv.ProjectionReceipt
	err := e.journaled(ctx, request.Fence, func() error {
		var err error
		receipt, err = e.journal.Apply(ctx, request)
		return err
	})
	return receipt, err
}

func (e *environment) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	var page execenv.ObservationPage
	err := e.journaled(ctx, fence, func() error {
		if err := e.journal.Bind(fence); err != nil {
			return err
		}
		var err error
		page, err = e.journal.Observe(ctx, fence, after, limit)
		return err
	})
	return page, err
}

func (e *environment) OpenObservationContent(ctx context.Context, fence execenv.ProjectionFence, ref execenv.ContentReference) (io.ReadCloser, error) {
	var body io.ReadCloser
	err := e.journaled(ctx, fence, func() error {
		var err error
		body, err = e.journal.Content(fence, ref)
		return err
	})
	return body, err
}

func (e *environment) AcknowledgeObservations(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	return e.journaled(ctx, fence, func() error { return e.journal.Acknowledge(fence, through) })
}

func (e *environment) ConfirmObservationProjection(ctx context.Context, binding execenv.ObservationProjection) error {
	return e.journaled(ctx, binding.Fence, func() error { return e.journal.Confirm(binding) })
}

// journaled runs fn under the fence and the environment's journal lock.
func (e *environment) journaled(ctx context.Context, fence execenv.ProjectionFence, fn func() error) error {
	return e.control.Run(ctx, fence, func() error {
		e.journalMu.Lock()
		defer e.journalMu.Unlock()
		return fn()
	})
}

// ReadGuest returns the body the environment currently sees at path. It is
// a test hook beside the guest writers below; production callers read guest
// changes only through Observe.
func (h *Host) ReadGuest(ctx context.Context, id execenv.ID, path string) ([]byte, error) {
	var body []byte
	err := h.guest(ctx, id, path, func(e *environment, full string) error {
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() {
			return execenv.ErrNotFound
		}
		body, err = os.ReadFile(full)
		return err
	})
	return body, err
}

// WriteGuest, MkdirGuest, RemoveGuest and MoveGuest change the workspace
// as the environment's own programs would. They are how tests of this
// adapter produce observations without a real guest. A parent must exist.
func (h *Host) WriteGuest(ctx context.Context, id execenv.ID, path string, data []byte) error {
	return h.guest(ctx, id, path, func(e *environment, full string) error {
		if err := e.parent(full); err != nil {
			return err
		}
		if info, err := os.Lstat(full); err == nil && info.IsDir() {
			return execenv.ErrConflict
		}
		return os.WriteFile(full, data, 0o644)
	})
}

// MkdirGuest creates one directory as the environment would.
func (h *Host) MkdirGuest(ctx context.Context, id execenv.ID, path string) error {
	return h.guest(ctx, id, path, func(e *environment, full string) error {
		if err := e.parent(full); err != nil {
			return err
		}
		if err := os.Mkdir(full, 0o755); errors.Is(err, fs.ErrExist) {
			return execenv.ErrConflict
		} else if err != nil {
			return err
		}
		return nil
	})
}

// RemoveGuest removes a file or a whole directory.
func (h *Host) RemoveGuest(ctx context.Context, id execenv.ID, path string) error {
	return h.guest(ctx, id, path, func(e *environment, full string) error {
		if _, err := os.Lstat(full); err != nil {
			return execenv.ErrNotFound
		}
		return os.RemoveAll(full)
	})
}

// MoveGuest renames from to to. The destination must not exist.
func (h *Host) MoveGuest(ctx context.Context, id execenv.ID, from, to string) error {
	if err := execenv.ValidatePath(to); err != nil {
		return execenv.Error("guest", err)
	}
	return h.guest(ctx, id, from, func(e *environment, full string) error {
		dest, err := execenv.ResolvePath(e.workspace, to)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(full); err != nil {
			return execenv.ErrNotFound
		}
		if _, err := os.Lstat(dest); err == nil {
			return execenv.ErrConflict
		}
		if err := e.parent(dest); err != nil {
			return err
		}
		return os.Rename(full, dest)
	})
}

// guest runs fn on path in a live grant's workspace, as its guest.
func (h *Host) guest(ctx context.Context, id execenv.ID, path string, fn func(e *environment, full string) error) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("guest", err)
	}
	if err := execenv.ValidatePath(path); err != nil {
		return execenv.Error("guest", err)
	}
	h.mu.Lock()
	e, ok := h.grants[id]
	h.mu.Unlock()
	if !ok {
		return execenv.Error("guest", execenv.ErrRevoked)
	}
	e.mu.Lock()
	err := e.guard("guest")
	e.mu.Unlock()
	if err != nil {
		return err
	}
	full, err := execenv.ResolvePath(e.workspace, path)
	if err != nil {
		return execenv.Error("guest", err)
	}
	e.guestMu.Lock()
	defer e.guestMu.Unlock()
	defer e.dirty.touch()
	if err := fn(e, full); err != nil {
		if errors.Is(err, execenv.ErrNotFound) || errors.Is(err, execenv.ErrConflict) {
			return execenv.Error("guest", err)
		}
		return execenv.Error("guest", execenv.ErrUnavailable)
	}
	return nil
}

func (e *environment) parent(full string) error {
	info, err := os.Lstat(filepath.Dir(full))
	if err != nil || !info.IsDir() {
		return execenv.ErrConflict
	}
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
