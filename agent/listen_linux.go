//go:build linux

package agent

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// ListenGuest binds the guest-side link the host dials after Ensure.
func ListenGuest() (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(LinkPort)}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := unix.Listen(fd, 8); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "guest")
	ln, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	return ln, nil
}
