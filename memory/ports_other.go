//go:build !unix && !windows

package memory

import "net"

func dualStack(*net.TCPListener) bool { return false }
