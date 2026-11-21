//go:build unix

package memory

import (
	"net"

	"golang.org/x/sys/unix"
)

func dualStack(listener *net.TCPListener) bool {
	raw, err := listener.SyscallConn()
	if err != nil {
		return false
	}
	var dual bool
	err = raw.Control(func(fd uintptr) {
		v6only, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
		dual = err == nil && v6only == 0
	})
	return err == nil && dual
}
