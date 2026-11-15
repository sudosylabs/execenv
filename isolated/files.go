package isolated

import (
	"context"
	"io"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
)

// The file API goes to the guest. Reads need a live grant; changes are
// checked against the fence first and stream outside the control lock, as
// uploads do, and the guest binds the fence again when it commits them.

func (e *environment) reader(op string) (*agent.Client, error) { return e.live(op) }

func (e *environment) writer(ctx context.Context, fence execenv.ProjectionFence) (*agent.Client, error) {
	var client *agent.Client
	err := e.control.Run(ctx, fence, func() error {
		var err error
		client, err = e.projectionClient()
		return err
	})
	return client, err
}

func (e *environment) Stat(ctx context.Context, path string, digest bool) (execenv.FileInfo, error) {
	c, err := e.reader("stat")
	if err != nil {
		return execenv.FileInfo{}, err
	}
	return c.Stat(ctx, path, digest)
}

func (e *environment) List(ctx context.Context, dir string, depth int, digest bool) ([]execenv.FileInfo, error) {
	c, err := e.reader("list")
	if err != nil {
		return nil, err
	}
	return c.List(ctx, dir, depth, digest)
}

func (e *environment) ReadFile(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	c, err := e.reader("read_file")
	if err != nil {
		return nil, err
	}
	return c.ReadFile(ctx, path, offset)
}

func (e *environment) Export(ctx context.Context, paths []string, codec string) (io.ReadCloser, error) {
	c, err := e.reader("export")
	if err != nil {
		return nil, err
	}
	return c.Export(ctx, paths, codec)
}

func (e *environment) WriteFile(ctx context.Context, fence execenv.ProjectionFence, path string, body io.Reader, opts execenv.WriteOptions) (execenv.FileInfo, error) {
	c, err := e.writer(ctx, fence)
	if err != nil {
		return execenv.FileInfo{}, err
	}
	return c.WriteFile(ctx, fence, path, body, opts)
}

func (e *environment) MakeDirectory(ctx context.Context, fence execenv.ProjectionFence, path string, parents bool) error {
	c, err := e.writer(ctx, fence)
	if err != nil {
		return err
	}
	return c.MakeDirectory(ctx, fence, path, parents)
}

func (e *environment) Move(ctx context.Context, fence execenv.ProjectionFence, from, to string) error {
	c, err := e.writer(ctx, fence)
	if err != nil {
		return err
	}
	return c.Move(ctx, fence, from, to)
}

func (e *environment) Remove(ctx context.Context, fence execenv.ProjectionFence, path string, recursive bool) error {
	c, err := e.writer(ctx, fence)
	if err != nil {
		return err
	}
	return c.Remove(ctx, fence, path, recursive)
}

func (e *environment) Import(ctx context.Context, fence execenv.ProjectionFence, dir string, archive io.Reader) error {
	c, err := e.writer(ctx, fence)
	if err != nil {
		return err
	}
	return c.Import(ctx, fence, dir, archive)
}
