//go:build !linux

package guestlink

import (
	"net"

	"github.com/sudosylabs/execenv"
)

// Listen is the guest's end of the host link. Only a Linux guest has that
// address family.
func Listen(uint32) (net.Listener, error) {
	return nil, execenv.ErrUnavailable
}
