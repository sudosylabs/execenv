package remote

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/wire"
)

// FeatureProcess is a host that runs processes by tag.
const FeatureProcess = "process"

// FeatureScreen is a host that reads a terminal process's screen.
const FeatureScreen = "screen"

// Host side. Every call is stateless: a process lives on the host, not on
// the connection that started it, and an attachment is one stream.

func (h *server) StartProcess(ctx context.Context, req *connect.Request[execenvv1.StartProcessRequest]) (*connect.Response[execenvv1.ProcessInfo], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	spec, ok := wire.ProcessSpecFrom(req.Msg.GetSpec())
	if !ok {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	info, err := env.Start(ctx, fence, spec)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ProcessInfoTo(info)), nil
}

func (h *server) GetProcess(ctx context.Context, req *connect.Request[execenvv1.GetProcessRequest]) (*connect.Response[execenvv1.ProcessInfo], error) {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return nil, wire.Error(err)
	}
	info, err := env.Process(ctx, req.Msg.GetTag())
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ProcessInfoTo(info)), nil
}

func (h *server) GetScreen(ctx context.Context, req *connect.Request[execenvv1.GetScreenRequest]) (*connect.Response[execenvv1.Screen], error) {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return nil, wire.Error(err)
	}
	s, err := env.Screen(ctx, req.Msg.GetTag())
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ScreenTo(s)), nil
}

func (h *server) ListProcesses(ctx context.Context, req *connect.Request[execenvv1.ListProcessesRequest]) (*connect.Response[execenvv1.ListProcessesResponse], error) {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return nil, wire.Error(err)
	}
	infos, err := env.ListProcesses(ctx)
	if err != nil {
		return nil, wire.Error(err)
	}
	out := &execenvv1.ListProcessesResponse{}
	for _, info := range infos {
		out.Processes = append(out.Processes, wire.ProcessInfoTo(info))
	}
	return connect.NewResponse(out), nil
}

// AttachProcess streams until the exit. Ending the stream, by the client or
// a lost connection, detaches; the process stays.
func (h *server) AttachProcess(ctx context.Context, req *connect.Request[execenvv1.AttachProcessRequest], stream *connect.ServerStream[execenvv1.OutputEvent]) error {
	env, err := h.env(execenv.ID(req.Msg.GetGrantId()), req.Msg.GetEpoch())
	if err != nil {
		return wire.Error(err)
	}
	att, err := env.AttachProcess(ctx, req.Msg.GetTag(), wire.OffsetsFrom(req.Msg.GetFrom()))
	if err != nil {
		return wire.Error(err)
	}
	defer att.Close()
	if err := stream.Send(&execenvv1.OutputEvent{Kind: execenvv1.OutputKind_OUTPUT_KIND_ATTACHED}); err != nil {
		return err
	}
	for {
		out, err := att.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return wire.Error(err)
		}
		if err := stream.Send(wire.OutputTo(out)); err != nil {
			return err
		}
	}
}

func (h *server) WriteInput(ctx context.Context, req *connect.Request[execenvv1.WriteInputRequest]) (*connect.Response[execenvv1.WriteInputResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	next, err := env.WriteInput(ctx, fence, req.Msg.GetTag(), req.Msg.GetOffset(), req.Msg.GetData())
	if err != nil {
		// A refusal still says where input stands, so the caller can resend.
		cerr := wire.Error(err).(*connect.Error)
		cerr.Meta().Set(inputOffsetHeader, strconv.FormatUint(next, 10))
		return nil, cerr
	}
	return connect.NewResponse(&execenvv1.WriteInputResponse{Offset: next}), nil
}

func (h *server) CloseInput(ctx context.Context, req *connect.Request[execenvv1.CloseInputRequest]) (*connect.Response[execenvv1.CloseInputResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	if err := env.CloseInput(ctx, fence, req.Msg.GetTag()); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.CloseInputResponse{}), nil
}

func (h *server) ResizeProcess(ctx context.Context, req *connect.Request[execenvv1.ResizeProcessRequest]) (*connect.Response[execenvv1.ResizeProcessResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	window, ok := wire.WindowFrom(req.Msg.GetWindow())
	if !ok {
		return nil, wire.Error(execenv.ErrInvalid)
	}
	if err := env.Resize(ctx, fence, req.Msg.GetTag(), window); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.ResizeProcessResponse{}), nil
}

func (h *server) SignalProcess(ctx context.Context, req *connect.Request[execenvv1.SignalProcessRequest]) (*connect.Response[execenvv1.SignalProcessResponse], error) {
	env, fence, err := h.fenced(req.Msg.GetFence())
	if err != nil {
		return nil, wire.Error(err)
	}
	if err := env.SignalProcess(ctx, fence, req.Msg.GetTag(), execenv.Signal(req.Msg.GetSignal())); err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(&execenvv1.SignalProcessResponse{}), nil
}

// Client side.

func (e *remoteEnv) Start(ctx context.Context, fence execenv.ProjectionFence, spec execenv.ProcessSpec) (execenv.ProcessInfo, error) {
	if err := e.guard(fence); err != nil {
		return execenv.ProcessInfo{}, err
	}
	if err := execenv.ValidateProcessSpec(spec); err != nil {
		return execenv.ProcessInfo{}, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.procs.StartProcess(ctx, connect.NewRequest(&execenvv1.StartProcessRequest{Fence: wire.FenceTo(fence), Spec: wire.ProcessSpecTo(spec)}))
	if err != nil {
		return execenv.ProcessInfo{}, wire.HostError("start", err)
	}
	info := wire.ProcessInfoFrom(resp.Msg)
	if info.Tag != spec.Tag {
		return execenv.ProcessInfo{}, execenv.ErrUnavailable
	}
	return info, nil
}

func (e *remoteEnv) Process(ctx context.Context, tag string) (execenv.ProcessInfo, error) {
	if err := e.alive(); err != nil {
		return execenv.ProcessInfo{}, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.procs.GetProcess(ctx, connect.NewRequest(&execenvv1.GetProcessRequest{GrantId: string(e.id), Epoch: e.epoch, Tag: tag}))
	if err != nil {
		return execenv.ProcessInfo{}, wire.HostError("process", err)
	}
	return wire.ProcessInfoFrom(resp.Msg), nil
}

func (e *remoteEnv) Screen(ctx context.Context, tag string) (execenv.Screen, error) {
	if err := e.alive(); err != nil {
		return execenv.Screen{}, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.procs.GetScreen(ctx, connect.NewRequest(&execenvv1.GetScreenRequest{GrantId: string(e.id), Epoch: e.epoch, Tag: tag}))
	if err != nil {
		return execenv.Screen{}, wire.HostError("screen", err)
	}
	return wire.ScreenFrom(resp.Msg), nil
}

func (e *remoteEnv) ListProcesses(ctx context.Context) ([]execenv.ProcessInfo, error) {
	if err := e.alive(); err != nil {
		return nil, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.procs.ListProcesses(ctx, connect.NewRequest(&execenvv1.ListProcessesRequest{GrantId: string(e.id), Epoch: e.epoch}))
	if err != nil {
		return nil, wire.HostError("processes", err)
	}
	out := make([]execenv.ProcessInfo, 0, len(resp.Msg.GetProcesses()))
	for _, p := range resp.Msg.GetProcesses() {
		out = append(out, wire.ProcessInfoFrom(p))
	}
	return out, nil
}

// AttachProcess opens the output stream on the interactive connection. It
// outlives ctx; Close, the exit, or the client's Close end it.
func (e *remoteEnv) AttachProcess(ctx context.Context, tag string, from map[execenv.Stream]uint64) (execenv.Attachment, error) {
	if err := e.alive(); err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(e.client.ctx)
	stream, err := e.client.procs.AttachProcess(sctx, connect.NewRequest(&execenvv1.AttachProcessRequest{GrantId: string(e.id), Epoch: e.epoch, Tag: tag, From: wire.OffsetsTo(from)}))
	if err != nil {
		cancel()
		return nil, wire.HostError("attach", err)
	}
	// The host attaches, or refuses, before AttachProcess returns.
	attached := make(chan error, 1)
	go func() {
		if !stream.Receive() {
			attached <- wire.HostError("attach", stream.Err())
			return
		}
		if stream.Msg().GetKind() != execenvv1.OutputKind_OUTPUT_KIND_ATTACHED {
			attached <- execenv.Error("attach", execenv.ErrUnavailable)
			return
		}
		attached <- nil
	}()
	select {
	case err = <-attached:
	case <-ctx.Done():
		cancel()
		<-attached
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		return nil, err
	}
	a := &remoteAttachment{env: e, stream: stream, cancel: cancel, events: make(chan attachEvent, 1)}
	go a.pump()
	e.mu.Lock()
	if e.atts == nil {
		e.atts = make(map[*remoteAttachment]struct{})
	}
	e.atts[a] = struct{}{}
	e.mu.Unlock()
	if e.revoked.Load() {
		a.end(execenv.ErrRevoked)
	}
	return a, nil
}

func (e *remoteEnv) endAttachments(err error) {
	e.mu.Lock()
	atts := make([]*remoteAttachment, 0, len(e.atts))
	for a := range e.atts {
		atts = append(atts, a)
	}
	e.mu.Unlock()
	for _, a := range atts {
		a.end(err)
	}
}

type attachEvent struct {
	out execenv.Output
	err error
}

// remoteAttachment pulls one event ahead of its reader, so HTTP/2 flow
// control holds the host back while the caller is slow.
type remoteAttachment struct {
	env    *remoteEnv
	stream *connect.ServerStreamForClient[execenvv1.OutputEvent]
	cancel context.CancelFunc
	events chan attachEvent

	mu   sync.Mutex
	dead error
	last error
}

func (a *remoteAttachment) pump() {
	defer close(a.events)
	for a.stream.Receive() {
		out, ok := wire.OutputFrom(a.stream.Msg())
		if !ok {
			a.events <- attachEvent{err: execenv.ErrUnavailable}
			return
		}
		a.events <- attachEvent{out: out}
	}
	err := a.stream.Err()
	switch {
	case err == nil:
		err = io.EOF
	case a.env.client.ctx.Err() != nil:
		err = execenv.Error("attach", execenv.ErrConnection)
	default:
		err = wire.HostError("attach", err)
	}
	a.events <- attachEvent{err: err}
}

func (a *remoteAttachment) Next(ctx context.Context) (execenv.Output, error) {
	a.mu.Lock()
	if a.dead != nil {
		err := a.dead
		a.mu.Unlock()
		return execenv.Output{}, err
	}
	if a.last != nil {
		err := a.last
		a.mu.Unlock()
		return execenv.Output{}, err
	}
	a.mu.Unlock()
	var ev attachEvent
	select {
	case got, ok := <-a.events:
		if !ok {
			return execenv.Output{}, a.deadOr(execenv.ErrClosed)
		}
		ev = got
	case <-ctx.Done():
		return execenv.Output{}, ctx.Err()
	}
	if ev.err != nil {
		a.mu.Lock()
		a.last = ev.err
		a.mu.Unlock()
		a.env.forget(a)
		return execenv.Output{}, a.deadOr(ev.err)
	}
	return ev.out, nil
}

func (a *remoteAttachment) deadOr(err error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dead != nil {
		return a.dead
	}
	return err
}

func (a *remoteAttachment) end(err error) {
	a.mu.Lock()
	if a.dead == nil {
		a.dead = err
	}
	a.mu.Unlock()
	a.cancel()
	a.env.forget(a)
}

// Close detaches. The process keeps running on the host.
func (a *remoteAttachment) Close() error {
	a.end(execenv.ErrClosed)
	// Let the pump finish so no goroutine outlives the attachment.
	go func() {
		for range a.events {
		}
	}()
	return nil
}

func (e *remoteEnv) forget(a *remoteAttachment) {
	e.mu.Lock()
	delete(e.atts, a)
	e.mu.Unlock()
}

// inputOffsetHeader carries where input stands when WriteInput fails.
const inputOffsetHeader = "Execenv-Input-Offset"

func (e *remoteEnv) WriteInput(ctx context.Context, fence execenv.ProjectionFence, tag string, offset uint64, data []byte) (uint64, error) {
	if err := e.guard(fence); err != nil {
		return 0, err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.procs.WriteInput(ctx, connect.NewRequest(&execenvv1.WriteInputRequest{Fence: wire.FenceTo(fence), Tag: tag, Offset: offset, Data: data}))
	if err != nil {
		var cerr *connect.Error
		var at uint64
		if errors.As(err, &cerr) {
			at, _ = strconv.ParseUint(cerr.Meta().Get(inputOffsetHeader), 10, 64)
		}
		return at, wire.HostError("write_input", err)
	}
	return resp.Msg.GetOffset(), nil
}

func (e *remoteEnv) CloseInput(ctx context.Context, fence execenv.ProjectionFence, tag string) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.procs.CloseInput(ctx, connect.NewRequest(&execenvv1.CloseInputRequest{Fence: wire.FenceTo(fence), Tag: tag}))
	return wire.HostError("close_input", err)
}

func (e *remoteEnv) Resize(ctx context.Context, fence execenv.ProjectionFence, tag string, w execenv.Window) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.procs.ResizeProcess(ctx, connect.NewRequest(&execenvv1.ResizeProcessRequest{Fence: wire.FenceTo(fence), Tag: tag, Window: wire.WindowTo(w)}))
	return wire.HostError("resize", err)
}

func (e *remoteEnv) SignalProcess(ctx context.Context, fence execenv.ProjectionFence, tag string, s execenv.Signal) error {
	if err := e.guard(fence); err != nil {
		return err
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	_, err := e.client.procs.SignalProcess(ctx, connect.NewRequest(&execenvv1.SignalProcessRequest{Fence: wire.FenceTo(fence), Tag: tag, Signal: string(s)}))
	return wire.HostError("signal", err)
}
