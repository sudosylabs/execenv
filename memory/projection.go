package memory

import (
	"context"
	"io"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/observations"
	"github.com/sudosylabs/execenv/internal/projection"
	"github.com/sudosylabs/execenv/internal/tree"
)

var _ execenv.ProjectionEnv = (*environment)(nil)

func (e *environment) UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, reader io.Reader) (execenv.ContentReference, error) {
	if reader == nil {
		return execenv.ContentReference{}, execenv.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return execenv.ContentReference{}, err
	}
	body, err := io.ReadAll(io.LimitReader(reader, execenv.MaxFileBytes+1))
	if err != nil {
		return execenv.ContentReference{}, err
	}
	if len(body) > execenv.MaxFileBytes {
		return execenv.ContentReference{}, execenv.ErrTooLarge
	}
	var reference execenv.ContentReference
	err = e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		var err error
		reference, err = e.projection.Put(body)
		return err
	})
	return reference, err
}

func (e *environment) InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	var receipt execenv.ProjectionReceipt
	err := e.control.Run(ctx, request.Fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		var err error
		receipt, err = e.projection.Initialize(request, func(snapshot execenv.Tree) error {
			if e.term != nil && !e.term.finished() || len(e.files) != 0 {
				return execenv.ErrConflict
			}
			next, err := tree.Replace(nil, snapshot)
			if err != nil {
				return err
			}
			e.files = next
			e.failWatch(execenv.ErrLagged)
			return nil
		})
		if err == nil && e.semantic == nil {
			e.projectionFence = request.Fence
			e.semantic = observations.New(e.id, e.Epoch())
			e.refreshGuestProjection(nil)
		}
		return err
	})
	return receipt, err
}

func (e *environment) ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	var receipt execenv.ProjectionReceipt
	err := e.control.Run(ctx, request.Fence, func() error {
		e.mu.Lock()
		defer e.mu.Unlock()
		var err error
		receipt, err = e.projection.Apply(request, func(plan projection.Plan) error {
			prepared, err := plan.Prepare(e.guestTree)
			if err != nil {
				return err
			}
			e.files = prepared.Tree
			e.guestTree = prepared.Guest
			return nil
		})
		return err
	})
	return receipt, err
}
