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
	Disk string
	// Seed, when set, is the directory of the seed the workspace starts as
	// a copy of, as the launcher's prepareSeed left it; otherwise the
	// workspace starts empty.
	Seed string
	// Resources size the machine: virtual CPUs enough for CPUMillis,
	// throttled to it; MemoryBytes of guest memory; a DiskBytes workspace
	// disk.
	Resources execenv.Resources
	// Limits throttle the machine's disk and network devices.
	Limits execenv.Limits
	Attach *netAttach
}

// instance is one running isolated environment. Pause must not destroy it.
// Connect is the guest link the host dials after Start; the public Host
// type does not name the transport. Done closes once the machine has
// exited, whether Stop ended it or it ended on its own (its monitor died,
// or the guest crashed and the machine reset). Stop still releases the
// host resources of a machine that already exited.
type instance interface {
	Pause(context.Context) error
	Resume(context.Context) error
	Stop(context.Context) error
	Connect(ctx context.Context) (net.Conn, error)
	Done() <-chan struct{}
	// UpdateLimits replaces the running machine's device limits; network
	// says whether it has a network device to limit.
	UpdateLimits(ctx context.Context, limits execenv.Limits, network bool) error
}

type launcher interface {
	Start(ctx context.Context, req startRequest) (instance, error)
}

// seedPreparer turns a freshly unpacked seed into what the launcher clones
// for each grant. A launcher without one starts grants from the seed's tree.
type seedPreparer interface {
	prepareSeed(stage string) error
}

// staleCleaner releases host resources a crashed daemon left behind. It
// runs once at construction, holding the work directory lock.
type staleCleaner interface {
	cleanStale() error
}
