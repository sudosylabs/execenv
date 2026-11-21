package memory

import (
	"context"
	"net"
	"net/netip"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/portconn"
)

type portListener struct {
	listener  *net.TCPListener
	tag       string
	dualStack bool
}

// RegisterPortListener exposes a local loopback or wildcard listener
// to calls on env. An optional tag identifies its owner in ListPorts.
// The caller owns the listener and its lifetime.
func (h *Host) RegisterPortListener(env execenv.Env, listener *net.TCPListener, tag ...string) error {
	if env == nil || !listenerOpen(listener) || len(tag) > 1 {
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
		e.listeners = make(map[netip.AddrPort]portListener)
	}
	registered := portListener{listener: listener}
	if len(tag) != 0 {
		registered.tag = tag[0]
	}
	if addr.Addr() == netip.IPv6Unspecified() {
		registered.dualStack = dualStack(listener)
	}
	e.listeners[addr] = registered
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
	var slot *portconn.Slot
	var ipv4, ipv6 bool
	err := e.control.Run(ctx, fence, func() error {
		e.mu.Lock()
		ipv4, ipv6 = e.portFamilies(uint16(port))
		e.mu.Unlock()
		release := e.timer.Hold()
		var err error
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
		return portconn.Dial(ctx, port, ipv4, ipv6)
	}, nil)
	return conn, execenv.Error("connect_port", err)
}

// portFamilies is called under e.mu.
func (e *environment) portFamilies(port uint16) (ipv4, ipv6 bool) {
	for _, addr := range []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv4Unspecified(), netip.IPv6Loopback(), netip.IPv6Unspecified()} {
		p := e.listeners[netip.AddrPortFrom(addr, port)]
		if listenerOpen(p.listener) {
			ipv4 = ipv4 || addr.Is4() || p.dualStack
			ipv6 = ipv6 || addr.Is6()
		}
	}
	return ipv4, ipv6
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
		if listenerOpen(listener.listener) {
			ports = append(ports, execenv.ListeningPort{Port: int(addr.Port()), Address: addr.Addr().String(), Tag: listener.tag})
		} else {
			delete(e.listeners, addr)
		}
	}
	return portconn.List(ports), nil
}
