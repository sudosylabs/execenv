package remote

import (
	"context"

	"github.com/sudosylabs/execenv"
)

const (
	methodProcessObserve     = "process_observe"
	methodProcessAcknowledge = "process_acknowledge"
)

// processArgs names one process log page or acknowledgement. The host
// checks the fence exactly as for observations.
type processArgs struct {
	Fence   execenv.ProjectionFence `json:"fence"`
	After   uint64                  `json:"after,omitempty"`
	Limit   int                     `json:"limit,omitempty"`
	Through uint64                  `json:"through,omitempty"`
}

func (e *remoteEnv) processCall(ctx context.Context, method string, args processArgs) ([]byte, error) {
	if err := e.projectionGuard(args.Fence); err != nil {
		return nil, err
	}
	extra, err := encodeExtra(args)
	if err != nil {
		return nil, err
	}
	return e.client.call(ctx, method, string(e.id), extra)
}

func (e *remoteEnv) ObserveProcesses(ctx context.Context, fence execenv.ProjectionFence, after uint64, limit int) (execenv.ProcessPage, error) {
	if limit < 1 || limit > execenv.MaxProcessPage || after > execenv.MaxProjectionCursor {
		return execenv.ProcessPage{}, execenv.ErrInvalid
	}
	raw, err := e.processCall(ctx, methodProcessObserve, processArgs{Fence: fence, After: after, Limit: limit})
	if err != nil {
		return execenv.ProcessPage{}, err
	}
	var page execenv.ProcessPage
	if len(raw) > execenv.MaxProjectionMetadataBytes || decodeExtra(raw, &page) != nil || len(page.Events) > limit || page.Next != after+uint64(len(page.Events)) {
		return execenv.ProcessPage{}, execenv.ErrUnavailable
	}
	for i, event := range page.Events {
		if execenv.ValidateProcessEvent(event) != nil || event.Sequence != after+uint64(i)+1 {
			return execenv.ProcessPage{}, execenv.ErrUnavailable
		}
	}
	if page.Events == nil {
		page.Events = []execenv.ProcessEvent{}
	}
	return page, nil
}

func (e *remoteEnv) AcknowledgeProcesses(ctx context.Context, fence execenv.ProjectionFence, through uint64) error {
	_, err := e.processCall(ctx, methodProcessAcknowledge, processArgs{Fence: fence, Through: through})
	return err
}

func (sc *serverConn) processes(ctx context.Context, f frame) ([]byte, error) {
	if len(f.Extra) > execenv.MaxProjectionMetadataBytes {
		return nil, execenv.ErrTooLarge
	}
	var args processArgs
	if decodeExtra(f.Extra, &args) != nil || execenv.ValidateProjectionFence(args.Fence) != nil || string(args.Fence.ExecutionGrantID) != f.Grant {
		return nil, execenv.ErrInvalid
	}
	env, err := sc.projectionEnvironment(ctx, f.Grant, args.Fence)
	if err != nil {
		return nil, err
	}
	switch f.Method {
	case methodProcessObserve:
		page, err := env.ObserveProcesses(ctx, args.Fence, args.After, args.Limit)
		if err != nil {
			return nil, err
		}
		return encodeExtra(page)
	case methodProcessAcknowledge:
		return nil, env.AcknowledgeProcesses(ctx, args.Fence, args.Through)
	default:
		return nil, execenv.ErrInvalid
	}
}
