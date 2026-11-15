//go:build linux

// Package guestlink listens on the guest's end of the host link.
package guestlink

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/sudosylabs/execenv"
	"golang.org/x/sys/unix"
)

// Listen binds port on the guest's end of the host link. Off a guest, or
// when the link family is missing, the error is ErrUnavailable.
// net.FileListener cannot wrap this family.
func Listen(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, execenv.ErrUnavailable
	}
	if err := unix.Listen(fd, 8); err != nil {
		_ = unix.Close(fd)
		return nil, execenv.ErrUnavailable
	}
	return &guestListener{f: os.NewFile(uintptr(fd), "guest"), port: port}, nil
}

type guestListener struct {
	f    *os.File
	port uint32
}

func (l *guestListener) Accept() (net.Conn, error) {
	if l == nil || l.f == nil {
		return nil, execenv.ErrUnavailable
	}
	raw, err := l.f.SyscallConn()
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	var nfd int
	var accErr error
	err = raw.Read(func(fd uintptr) bool {
		nfd, _, accErr = unix.Accept4(int(fd), unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		if accErr == unix.EAGAIN || accErr == unix.EWOULDBLOCK {
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if accErr != nil {
		return nil, accErr
	}
	return &guestConn{f: os.NewFile(uintptr(nfd), "guest")}, nil
}

func (l *guestListener) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

func (l *guestListener) Addr() net.Addr {
	return guestAddr{port: l.port}
}

type guestConn struct {
	f *os.File
}

func (c *guestConn) Read(b []byte) (int, error)  { return c.f.Read(b) }
func (c *guestConn) Write(b []byte) (int, error) { return c.f.Write(b) }
func (c *guestConn) Close() error                { return c.f.Close() }
func (c *guestConn) LocalAddr() net.Addr         { return guestAddr{} }
func (c *guestConn) RemoteAddr() net.Addr        { return guestAddr{} }
func (c *guestConn) SetDeadline(t time.Time) error {
	return c.f.SetDeadline(t)
}
func (c *guestConn) SetReadDeadline(t time.Time) error {
	return c.f.SetReadDeadline(t)
}
func (c *guestConn) SetWriteDeadline(t time.Time) error {
	return c.f.SetWriteDeadline(t)
}

type guestAddr struct{ port uint32 }

func (guestAddr) Network() string  { return "guest" }
func (a guestAddr) String() string { return fmt.Sprintf("%d", a.port) }
