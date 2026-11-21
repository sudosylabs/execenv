package memory

import (
	"net"
	"syscall"
)

func dualStack(listener *net.TCPListener) bool {
	raw, err := listener.SyscallConn()
	if err != nil {
		return false
	}
	var dual bool
	err = raw.Control(func(fd uintptr) {
		v6only, err := syscall.GetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY)
		dual = err == nil && v6only == 0
	})
	return err == nil && dual
}
