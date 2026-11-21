package agent

import (
	"context"

	"connectrpc.com/connect"

	"github.com/sudosylabs/execenv"
	guestv1 "github.com/sudosylabs/execenv/internal/gen/execenv/guest/v1"
	execenvv1 "github.com/sudosylabs/execenv/internal/gen/execenv/v1"
	"github.com/sudosylabs/execenv/internal/portconn"
	"github.com/sudosylabs/execenv/internal/wire"
)

// DialPort opens a byte stream to the guest's IPv4 loopback.
func (c *Client) DialPort(ctx context.Context, port int) (execenv.PortConn, error) {
	if !portconn.Valid(port) {
		return nil, execenv.ErrInvalid
	}
	if err := c.ioGuard(); err != nil {
		return nil, err
	}
	return wire.OpenPort(ctx, c.ctx, callTimeout, c.agent.DialPort, &execenvv1.PortOpen{Port: int32(port)}, func(err error) error {
		return c.failure("dial_port", err)
	})
}

func (s *server) DialPort(ctx context.Context, stream *connect.BidiStream[execenvv1.PortFrame, execenvv1.PortFrame]) error {
	return wire.ServePort(ctx, stream, func(open *execenvv1.PortOpen) (execenv.PortConn, error) {
		s.mu.Lock()
		err := s.guard()
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if open.GetFence() != nil {
			return nil, execenv.ErrInvalid
		}
		return portconn.Dial(ctx, int(open.GetPort()))
	})
}

// ListeningPorts reports the guest's listening TCP ports.
func (c *Client) ListeningPorts(ctx context.Context) ([]execenv.ListeningPort, error) {
	if err := c.ioGuard(); err != nil {
		return nil, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	resp, err := c.agent.ListeningPorts(ctx, connect.NewRequest(&guestv1.Empty{}))
	if err != nil {
		return nil, c.failure("list_ports", err)
	}
	return wire.ListeningPortsFrom(resp.Msg), nil
}

func (s *server) ListeningPorts(ctx context.Context, _ *connect.Request[guestv1.Empty]) (*connect.Response[execenvv1.ListeningPortsResponse], error) {
	s.mu.Lock()
	err := s.guard()
	s.mu.Unlock()
	if err != nil {
		return nil, wire.Error(err)
	}
	ports, err := listeningPorts(ctx, s.portTag)
	if err != nil {
		return nil, wire.Error(err)
	}
	return connect.NewResponse(wire.ListeningPortsTo(ports)), nil
}

func (s *server) portTag(pid int) string {
	if l, ok := s.launchOf(pid); ok {
		return l.tag
	}
	if s.processTag != nil {
		return s.processTag(pid)
	}
	return ""
}
