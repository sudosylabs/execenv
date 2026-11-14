package isolated

import (
	"context"

	"github.com/sudosylabs/execenv"
)

// The guest helper sends every process event as soon as it is taken; the
// agent client retains them on the host until the caller acknowledges.

func (e *environment) ObserveProcesses(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ProcessPage, error) {
	var page execenv.ProcessPage
	err := e.control.Run(ctx, fence, func() error {
		client, err := e.live("processes")
		if err != nil {
			return err
		}
		page, err = client.ObserveProcesses(ctx, after, limit)
		return err
	})
	return page, err
}

func (e *environment) AcknowledgeProcesses(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	return e.control.Run(ctx, fence, func() error {
		client, err := e.live("processes")
		if err != nil {
			return err
		}
		return client.AcknowledgeProcesses(ctx, through)
	})
}
