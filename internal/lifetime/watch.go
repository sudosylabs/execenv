package lifetime

import (
	"context"
	"io"
	"sync"

	"github.com/sudosylabs/execenv"
)

// Watch wraps env so every call on it is activity, and every attached
// terminal keeps it busy until it is closed, hung up or ends.
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

func (w *watched) Attach(ctx context.Context, request execenv.AttachRequest) (execenv.Terminal, error) {
	w.t.Touch()
	term, err := w.Env.Attach(ctx, request)
	if err != nil {
		return nil, err
	}
	return &heldTerminal{Terminal: term, t: w.t, release: w.t.Hold()}, nil
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

func (w *watched) Observe(ctx context.Context, f execenv.ProjectionFence, after uint64, limit int) (execenv.ObservationPage, error) {
	w.t.Touch()
	return w.Env.Observe(ctx, f, after, limit)
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

func (w *watched) ObserveProcesses(ctx context.Context, f execenv.ProjectionFence, after uint64, limit int) (execenv.ProcessPage, error) {
	w.t.Touch()
	return w.Env.ObserveProcesses(ctx, f, after, limit)
}

func (w *watched) AcknowledgeProcesses(ctx context.Context, f execenv.ProjectionFence, through uint64) error {
	w.t.Touch()
	return w.Env.AcknowledgeProcesses(ctx, f, through)
}

func (w *watched) UpdateLimits(ctx context.Context, f execenv.ProjectionFence, l execenv.Limits) error {
	w.t.Touch()
	return w.Env.UpdateLimits(ctx, f, l)
}

type heldTerminal struct {
	execenv.Terminal
	t       *Timer
	release func()
	once    sync.Once
}

func (h *heldTerminal) end() { h.once.Do(h.release) }

func (h *heldTerminal) Read(p []byte) (int, error) {
	n, err := h.Terminal.Read(p)
	if err != nil {
		h.end()
	}
	return n, err
}

func (h *heldTerminal) Write(p []byte) (int, error) {
	h.t.Touch()
	return h.Terminal.Write(p)
}

func (h *heldTerminal) Close() error {
	h.end()
	return h.Terminal.Close()
}

func (h *heldTerminal) Hangup(ctx context.Context) error {
	h.end()
	return h.Terminal.Hangup(ctx)
}
