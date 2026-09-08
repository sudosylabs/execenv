package isolated

import (
	"bytes"
	"context"
	"io"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
)

var _ execenv.SemanticEnv = (*environment)(nil)

func (e *environment) projectionClient() (*agent.Client, error) {
	client, err := e.live("projection")
	if err != nil {
		return nil, err
	}
	if !client.JournalEnabled() {
		return nil, execenv.ErrUnavailable
	}
	return client, nil
}
func (e *environment) UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, reader io.Reader) (execenv.ContentReference, error) {
	if reader == nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	if execenv.ValidateProjectionFence(fence) != nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return execenv.ContentReference{}, err
	}
	// Do not hold the containment serializer while consuming caller-owned I/O.
	body, err := io.ReadAll(io.LimitReader(reader, execenv.MaxFileBytes+1))
	if err != nil {
		return execenv.ContentReference{}, err
	}
	if len(body) > execenv.MaxFileBytes {
		return execenv.ContentReference{}, execenv.ErrTooLarge
	}
	var reference execenv.ContentReference
	err = e.control.Run(ctx, fence, func() error {
		client, err := e.projectionClient()
		if err != nil {
			return err
		}
		reference, err = client.UploadProjectionContent(ctx, fence, bytes.NewReader(body))
		return err
	})
	return reference, err
}
func (e *environment) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	var receipt execenv.ProjectionReceipt
	err := e.control.Run(ctx, request.Fence, func() error {
		client, err := e.projectionClient()
		if err != nil {
			return err
		}
		receipt, err = client.InitializeProjection(ctx, request)
		return err
	})
	return receipt, err
}
func (e *environment) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	var receipt execenv.ProjectionReceipt
	err := e.control.Run(ctx, request.Fence, func() error {
		client, err := e.projectionClient()
		if err != nil {
			return err
		}
		receipt, err = client.ApplyProjection(ctx, request)
		return err
	})
	return receipt, err
}
func (e *environment) Observe(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	var page execenv.ObservationPage
	err := e.control.Run(ctx, fence, func() error {
		client, err := e.projectionClient()
		if err != nil {
			return err
		}
		page, err = client.Observe(ctx, fence, after, limit)
		return err
	})
	return page, err
}
func (e *environment) AcknowledgeObservations(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	return e.control.Run(ctx, fence, func() error {
		client, err := e.projectionClient()
		if err != nil {
			return err
		}
		return client.AcknowledgeObservations(ctx, fence, through)
	})
}
func (e *environment) OpenObservationContent(ctx context.Context, fence execenv.ProjectionFence, reference execenv.ContentReference) (io.ReadCloser, error) {
	var reader io.ReadCloser
	err := e.control.Run(ctx, fence, func() error {
		client, err := e.projectionClient()
		if err != nil {
			return err
		}
		reader, err = client.OpenObservationContent(ctx, fence, reference)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &fencedReader{env: e, ctx: ctx, fence: fence, inner: reader}, nil
}

type fencedReader struct {
	env   *environment
	ctx   context.Context
	fence execenv.ProjectionFence
	inner io.ReadCloser
}

func (r *fencedReader) Read(p []byte) (int, error) {
	var n int
	err := r.env.control.Run(r.ctx, r.fence, func() error { var err error; n, err = r.inner.Read(p); return err })
	return n, err
}
func (r *fencedReader) Close() error { return r.inner.Close() }

func (e *environment) ConfirmObservationProjection(ctx context.Context, binding execenv.ObservationProjection) error {
	return e.control.Run(ctx, binding.Fence, func() error {
		client, err := e.projectionClient()
		if err != nil {
			return err
		}
		return client.ConfirmObservationProjection(ctx, binding)
	})
}
