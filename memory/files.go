package memory

import (
	"bytes"
	"context"
	"io"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/archive"
)

// The file API works on the grant's workspace directory. Changes take the
// simulated guest's lock, as the guest's own writes do, so a capture never
// sees one half-done.

func (e *environment) live(op string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lost {
		return execenv.Error(op, execenv.ErrLost)
	}
	if e.revoked {
		return execenv.Error(op, execenv.ErrRevoked)
	}
	return nil
}

func (e *environment) change(ctx context.Context, fence execenv.ProjectionFence, fn func() error) error {
	return e.control.Run(ctx, fence, func() error {
		e.guestMu.Lock()
		defer e.guestMu.Unlock()
		defer e.dirty.touch()
		return fn()
	})
}

func (e *environment) Stat(ctx context.Context, path string, digest bool) (execenv.FileInfo, error) {
	if err := e.live("stat"); err != nil {
		return execenv.FileInfo{}, err
	}
	return e.files.Stat(path, digest)
}

func (e *environment) List(ctx context.Context, dir string, depth int, digest bool) ([]execenv.FileInfo, error) {
	if err := e.live("list"); err != nil {
		return nil, err
	}
	return e.files.List(dir, depth, digest)
}

func (e *environment) ReadFile(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	if err := e.live("read_file"); err != nil {
		return nil, err
	}
	return e.files.ReadFile(path, offset)
}

func (e *environment) Export(ctx context.Context, paths []string, codec string) (io.ReadCloser, error) {
	if err := e.live("export"); err != nil {
		return nil, err
	}
	if codec == "" {
		codec = string(archive.CodecZstd)
	}
	var buf bytes.Buffer
	if err := e.files.Export(paths, archive.Codec(codec), &buf); err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), nil
}

func (e *environment) WriteFile(ctx context.Context, fence execenv.ProjectionFence, path string, body io.Reader, opts execenv.WriteOptions) (execenv.FileInfo, error) {
	var info execenv.FileInfo
	err := e.change(ctx, fence, func() error {
		var err error
		info, err = e.files.WriteFile(path, body, opts)
		return err
	})
	return info, err
}

func (e *environment) TransferOffset(ctx context.Context, transferID string) (int64, error) {
	if err := e.live("transfer_offset"); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return e.files.TransferOffset(transferID)
}

func (e *environment) MakeDirectory(ctx context.Context, fence execenv.ProjectionFence, path string, parents bool) error {
	return e.change(ctx, fence, func() error { return e.files.MakeDirectory(path, parents) })
}

func (e *environment) Move(ctx context.Context, fence execenv.ProjectionFence, from, to string) error {
	return e.change(ctx, fence, func() error { return e.files.Move(from, to) })
}

func (e *environment) Remove(ctx context.Context, fence execenv.ProjectionFence, path string, recursive bool) error {
	return e.change(ctx, fence, func() error { return e.files.Remove(path, recursive) })
}

func (e *environment) Import(ctx context.Context, fence execenv.ProjectionFence, dir string, r io.Reader) error {
	return e.change(ctx, fence, func() error { return e.files.Import(dir, r) })
}
