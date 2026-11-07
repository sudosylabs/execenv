package isolated

import (
	"context"
	"net"

	"github.com/sudosylabs/execenv"
)

// workspaceName is the grant-relative directory beside the grant's other
// host-side files. The guest sees the projected tree at
// execenv.GuestWorkspace (its working directory) and its scratch $HOME at
// execenv.GuestHome; both live on the grant's writable disk (Disk), and
// the image root filesystem stays read-only.
const workspaceName = "workspace"

type startRequest struct {
	ID      execenv.ID
	Kernel  string
	Rootfs  string
	TreeDir string
	// Disk is the host path of the grant's sparse writable workspace disk,
	// created by the launcher and removed with the grant directory.
	Disk      string
	DiskBytes int64
	Attach    *netAttach
	Memory    int64
	CPU       int
}

// instance is one running isolated environment. Pause must not destroy it.
// Connect is the guest link the host dials after Start; the public Host
// type does not name the transport.
type instance interface {
	Pause(context.Context) error
	Resume(context.Context) error
	Stop(context.Context) error
	Connect(ctx context.Context) (net.Conn, error)
}

type launcher interface {
	Start(ctx context.Context, req startRequest) (instance, error)
}

// staleCleaner releases host resources a crashed daemon left behind. It
// runs once at construction, holding the work directory lock.
type staleCleaner interface {
	cleanStale() error
}
