package remote

import (
	"context"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/portconn"
	"github.com/sudosylabs/execenv/internal/wire"
)

func (e *remoteEnv) ConnectPort(ctx context.Context, fence execenv.ProjectionFence, port int) (execenv.PortConn, error) {
	if !portconn.Valid(port) {
		return nil, execenv.Error("connect_port", execenv.ErrInvalid)
	}
	if err := e.guard(fence); err != nil {
		return nil, execenv.Error("connect_port", err)
	}
	c := e.client
	open := &execenvv1.PortOpen{Fence: wire.FenceTo(fence), Port: int32(port)}
	return wire.OpenPort(ctx, c.ctx, operationTimeoutOrDefault(c.cfg.OperationTimeout), c.ports.ConnectPort, open, func(err error) error {
		return wire.HostError("connect_port", err)
	})
}

func (h *server) ConnectPort(ctx context.Context, stream *connect.BidiStream[execenvv1.PortFrame, execenvv1.PortFrame]) error {
	return wire.ServePort(ctx, stream, func(open *execenvv1.PortOpen) (execenv.PortConn, error) {
		env, fence, err := h.fenced(open.GetFence())
		if err != nil {
			return nil, err
		}
		return env.ConnectPort(ctx, fence, int(open.GetPort()))
	})
}

func (e *remoteEnv) ListPorts(ctx context.Context) ([]execenv.ListeningPort, error) {
	if err := e.alive(); err != nil {
		return nil, execenv.Error("list_ports", err)
	}
	ctx, cancel := e.client.call(ctx)
	defer cancel()
	resp, err := e.client.ports.ListPorts(ctx, connect.NewRequest(&execenvv1.ListPortsRequest{Id: string(e.id), Epoch: e.epoch}))
	if err != nil {
		return nil, wire.HostError("list_ports", err)
	}
	return wire.ListeningPortsFrom(resp.Msg), nil
}

func (h *server) ListPorts(ctx context.Context, req *connect.Request[execenvv1.ListPortsRequest]) (*connect.Response[execenvv1.ListeningPortsResponse], error) {
	env, err := h.env(execenv.ID(req.Msg.GetId()), req.Msg.GetEpoch())
	if err != nil {
		return nil, wire.Error(err)
	}
	ports, err := env.ListPorts(ctx)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ListeningPortsTo(ports)), nil
}
