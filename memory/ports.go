package memory

import (
	"context"
	"net"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/portconn"
)

// RegisterPortListener exposes a local IPv4 loopback or wildcard listener
// to calls on env. The caller owns the listener and its lifetime.
func (h *Host) RegisterPortListener(env execenv.Env, listener *net.TCPListener) error {
	if listener == nil {
		return execenv.ErrInvalid
	}
	addr := listener.Addr().(*net.TCPAddr)
	if addr.IP.To4() == nil || !(addr.IP.IsUnspecified() || addr.IP.Equal(net.IPv4(127, 0, 0, 1))) {
		return execenv.ErrInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.grants[env.ID()]
	if e == nil || e.Epoch() != env.Epoch() {
		return execenv.ErrConflict
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.revoked {
		return e.guard("register_port_listener")
	}
	if e.listeners == nil {
		e.listeners = make(map[int]*net.TCPListener)
	}
	e.listeners[addr.Port] = listener
	return nil
}

func listenerOpen(listener *net.TCPListener) bool {
	if listener == nil {
		return false
	}
	raw, err := listener.SyscallConn()
	return err == nil && raw.Control(func(uintptr) {}) == nil
}

func (e *environment) ConnectPort(ctx context.Context, fence execenv.ProjectionFence, port int) (execenv.PortConn, error) {
	if !portconn.Valid(port) {
		return nil, execenv.Error("connect_port", execenv.ErrInvalid)
	}
	var conn execenv.PortConn
	err := e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		listener := e.listeners[port]
		e.mu.Unlock()
		if !listenerOpen(listener) {
			return execenv.ErrRefused
		}
		raw, err := portconn.Dial(ctx, port)
		if err != nil {
			return err
		}
		conn = e.ports.Add(ctx, raw, nil)
		return nil
	})
	return conn, execenv.Error("connect_port", err)
}
