package memory

import (
	"context"
	"net"
	"net/netip"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/portconn"
)

// RegisterPortListener exposes a local loopback or wildcard listener
// to calls on env. The caller owns the listener and its lifetime.
func (h *Host) RegisterPortListener(env execenv.Env, listener *net.TCPListener) error {
	if env == nil || !listenerOpen(listener) {
		return execenv.ErrInvalid
	}
	addr := listener.Addr().(*net.TCPAddr).AddrPort()
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	if portconn.AddressOrder(addr.Addr().String()) == 0 {
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
		e.listeners = make(map[netip.AddrPort]*net.TCPListener)
	}
	e.listeners[addr] = listener
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
		registered := e.hasPortListener(uint16(port))
		e.mu.Unlock()
		if e.ports.Full() {
			return execenv.ErrCapacity
		}
		if !registered {
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

// hasPortListener is called under e.mu.
func (e *environment) hasPortListener(port uint16) bool {
	for _, addr := range []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1}), netip.IPv4Unspecified()} {
		if listenerOpen(e.listeners[netip.AddrPortFrom(addr, port)]) {
			return true
		}
	}
	return false
}

func (e *environment) ListPorts(ctx context.Context) ([]execenv.ListeningPort, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("list_ports", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.guard("list_ports"); err != nil {
		return nil, err
	}
	var ports []execenv.ListeningPort
	for addr, listener := range e.listeners {
		if listenerOpen(listener) {
			ports = append(ports, execenv.ListeningPort{Port: int(addr.Port()), Address: addr.Addr().String()})
		} else {
			delete(e.listeners, addr)
		}
	}
	return portconn.List(ports), nil
}
