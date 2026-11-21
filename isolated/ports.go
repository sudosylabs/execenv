package isolated

import (
	"context"
	"errors"

	"github.com/sudosylabs/execenv/agent"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/portconn"
)

func (e *environment) ConnectPort(ctx context.Context, fence execenv.ProjectionFence, port int) (execenv.PortConn, error) {
	if !portconn.Valid(port) {
		return nil, execenv.Error("connect_port", execenv.ErrInvalid)
	}
	var slot *portconn.Slot
	var client *agent.Client
	err := e.control.Run(ctx, fence, func() error {
		var err error
		client, err = e.live("connect_port")
		if err != nil {
			return err
		}
		release := e.timer.Hold()
		slot, err = e.ports.Reserve(ctx, release)
		if err != nil {
			release()
		}
		return err
	})
	if err != nil {
		return nil, execenv.Error("connect_port", err)
	}
	conn, err := slot.Dial(func(ctx context.Context) (execenv.PortConn, error) {
		return client.DialPort(ctx, port)
	}, func(err error) error {
		return e.portError(client, err)
	})
	return conn, execenv.Error("connect_port", err)
}

func (e *environment) portError(client *agent.Client, err error) error {
	if errors.Is(err, execenv.ErrConnection) || errors.Is(err, execenv.ErrUnavailable) {
		select {
		case <-client.Done():
			return e.endCause(err)
		default:
		}
	}
	return err
}

func (e *environment) ListPorts(ctx context.Context) ([]execenv.ListeningPort, error) {
	client, err := e.live("list_ports")
	if err != nil {
		return nil, err
	}
	ports, err := client.ListeningPorts(ctx)
	return ports, execenv.Error("list_ports", e.portError(client, err))
}
