//go:build linux

// Package unixsock dials and binds Unix sockets at paths of any length. A
// socket address holds at most 107 bytes; a longer path is reached through
// a descriptor of its directory, so a deep work directory still works.
package unixsock

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// maxPath is the longest path a Unix socket address holds.
const maxPath = len(unix.RawSockaddrUnix{}.Path) - 1

// Dial connects to the socket at path.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	if len(path) <= maxPath {
		return d.DialContext(ctx, "unix", path)
	}
	var conn net.Conn
	err := viaDir(path, func(short string) error {
		var err error
		conn, err = d.DialContext(ctx, "unix", short)
		return err
	})
	return conn, err
}

// Listen binds a socket at path. The caller removes the file when done:
// the listener does not, since it may have bound a path it cannot name.
func Listen(path string) (net.Listener, error) {
	var ln net.Listener
	bind := func(name string) error {
		var err error
		ln, err = net.Listen("unix", name)
		return err
	}
	var err error
	if len(path) <= maxPath {
		err = bind(path)
	} else {
		err = viaDir(path, bind)
	}
	if err != nil {
		return nil, err
	}
	if u, ok := ln.(*net.UnixListener); ok {
		u.SetUnlinkOnClose(false)
	}
	return ln, nil
}

// viaDir calls fn with a short name for path through its directory.
func viaDir(path string, fn func(string) error) error {
	fd, err := unix.Open(filepath.Dir(path), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: filepath.Dir(path), Err: err}
	}
	defer unix.Close(fd)
	return fn("/proc/self/fd/" + strconv.Itoa(fd) + "/" + filepath.Base(path))
}
