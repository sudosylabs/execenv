package isolated

import (
	"context"
	"net"

	"github.com/sudosylabs/execenv"
)

// workspaceName is the grant-relative directory that holds the projected
// tree. The isolated environment treats this path as home and cwd; the
// root filesystem of the image stays read-only.
const workspaceName = "workspace"

type startRequest struct {
	ID      execenv.ID
	Kernel  string
	Rootfs  string
	TreeDir string
	Attach  *netAttach
	Memory  int64
	CPU     int
}

// instance is one running isolated environment. Pause must not destroy it.
// Connect is the guest link the host dials after Start; the public Host
// type does not name the transport.
type instance interface {
	Pause() error
	Resume() error
	Stop() error
	Connect(ctx context.Context) (net.Conn, error)
}

type launcher interface {
	Start(ctx context.Context, req startRequest) (instance, error)
}
