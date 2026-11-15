//go:build !linux

// Package unixsock dials and binds Unix sockets at paths of any length. A
// socket address holds about a hundred bytes; on Linux a longer path is
// reached through a descriptor of its directory.
package unixsock

import (
	"context"
	"net"
)

// Dial connects to the socket at path.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// Listen binds a socket at path. The caller removes the file when done.
func Listen(path string) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if u, ok := ln.(*net.UnixListener); ok {
		u.SetUnlinkOnClose(false)
	}
	return ln, nil
}
