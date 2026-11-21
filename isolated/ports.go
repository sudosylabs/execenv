package isolated

import (
	"context"
	"errors"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/portconn"
)

func (e *environment) ConnectPort(ctx context.Context, fence execenv.ProjectionFence, port int) (execenv.PortConn, error) {
	if !portconn.Valid(port) {
		return nil, execenv.Error("connect_port", execenv.ErrInvalid)
	}
	var conn execenv.PortConn
	err := e.control.Run(ctx, fence, func() error {
		client, err := e.live("connect_port")
		if err != nil {
			return err
		}
		if e.ports.Full() {
			return execenv.ErrCapacity
		}
		raw, err := client.DialPort(ctx, port)
		if err != nil {
			return err
		}
		conn = e.ports.Add(ctx, raw, e.portError)
		return nil
	})
	// The loss monitor needs the control gate to publish its verdict.
	return conn, execenv.Error("connect_port", e.portError(err))
}

func (e *environment) portError(err error) error {
	if errors.Is(err, execenv.ErrConnection) || errors.Is(err, execenv.ErrUnavailable) {
		return e.endCause(err)
	}
	return err
}

func (e *environment) ListPorts(ctx context.Context) ([]execenv.ListeningPort, error) {
	client, err := e.live("list_ports")
	if err != nil {
		return nil, err
	}
	ports, err := client.ListeningPorts(ctx)
	return ports, execenv.Error("list_ports", e.portError(err))
}
