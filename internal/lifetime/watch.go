package lifetime

import (
	"context"
	"io"
	"sync"

	"github.com/sudosylabs/execenv"
)

// Watch marks activity for fenced mutations and holds the timer for process
// attachments. Observation and port discovery pass through. The adapters
// hold the timer for the lifetime of each port connection.
func Watch(env execenv.Env, t *Timer) execenv.Env {
	if t == nil {
		return env
	}
	return &watched{Env: env, t: t}
}

type watched struct {
	execenv.Env
	t *Timer
}

func (w *watched) AttachProcess(ctx context.Context, tag string, from map[execenv.Stream]uint64) (execenv.Attachment, error) {
	w.t.Touch()
	a, err := w.Env.AttachProcess(ctx, tag, from)
	if err != nil {
		return nil, err
	}
	return &heldAttachment{Attachment: a, release: w.t.Hold()}, nil
}

func (w *watched) Start(ctx context.Context, f execenv.ProjectionFence, spec execenv.ProcessSpec) (execenv.ProcessInfo, error) {
	w.t.Touch()
	return w.Env.Start(ctx, f, spec)
}

func (w *watched) WriteInput(ctx context.Context, f execenv.ProjectionFence, tag string, offset uint64, data []byte) (uint64, error) {
	w.t.Touch()
	return w.Env.WriteInput(ctx, f, tag, offset, data)
}

func (w *watched) Control(ctx context.Context, r execenv.ControlRequest) (execenv.ControlReceipt, error) {
	w.t.Touch()
	return w.Env.Control(ctx, r)
}

func (w *watched) UploadProjectionContent(ctx context.Context, f execenv.ProjectionFence, body io.Reader) (execenv.ContentReference, error) {
	w.t.Touch()
	return w.Env.UploadProjectionContent(ctx, f, body)
}

func (w *watched) InitializeProjection(ctx context.Context, r execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error) {
	w.t.Touch()
	return w.Env.InitializeProjection(ctx, r)
}

func (w *watched) ApplyProjection(ctx context.Context, r execenv.ProjectionApply) (execenv.ProjectionReceipt, error) {
	w.t.Touch()
	return w.Env.ApplyProjection(ctx, r)
}

func (w *watched) OpenObservationContent(ctx context.Context, f execenv.ProjectionFence, c execenv.ContentReference) (io.ReadCloser, error) {
	w.t.Touch()
	return w.Env.OpenObservationContent(ctx, f, c)
}

func (w *watched) ConfirmObservationProjection(ctx context.Context, b execenv.ObservationProjection) error {
	w.t.Touch()
	return w.Env.ConfirmObservationProjection(ctx, b)
}

func (w *watched) AcknowledgeObservations(ctx context.Context, f execenv.ProjectionFence, through uint64) error {
	w.t.Touch()
	return w.Env.AcknowledgeObservations(ctx, f, through)
}

func (w *watched) AcknowledgeProcesses(ctx context.Context, f execenv.ProjectionFence, through uint64) error {
	w.t.Touch()
	return w.Env.AcknowledgeProcesses(ctx, f, through)
}

func (w *watched) UpdateLimits(ctx context.Context, f execenv.ProjectionFence, l execenv.Limits) error {
	w.t.Touch()
	return w.Env.UpdateLimits(ctx, f, l)
}

type heldAttachment struct {
	execenv.Attachment
	release func()
	once    sync.Once
}

func (h *heldAttachment) Next(ctx context.Context) (execenv.Output, error) {
	out, err := h.Attachment.Next(ctx)
	if err != nil {
		h.once.Do(h.release)
	}
	return out, err
}

func (h *heldAttachment) Close() error {
	h.once.Do(h.release)
	return h.Attachment.Close()
}
