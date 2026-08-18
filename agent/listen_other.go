//go:build !linux

package agent

import (
	"net"

	"github.com/sudosylabs/execenv"
)

// ListenGuest is the guest-side link. Only a Linux guest has that
// address family; tests use a Unix socket via ListenAndServe.
func ListenGuest() (net.Listener, error) {
	return nil, execenv.ErrUnavailable
}
