// Package isolated is an execenv host that occupies each grant in its own
// microVM. Exported names describe isolation, not a particular hypervisor.
package isolated

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/sudosylabs/execenv"
)

var (
	_ execenv.Host        = (*Host)(nil)
	_ execenv.Env         = (*environment)(nil)
	_ execenv.GuestWriter = (*Host)(nil)
)

// Image is one catalog entry. Kernel and Rootfs are local paths to the
// read-only boot artifacts. Hash is the lowercase hex SHA-256 of Rootfs.
// Images are never pulled; a missing or mismatched file is simply absent.
type Image struct {
	ID     execenv.Image
	Kernel string
	Rootfs string
	Hash   string
}

// Config constructs an isolated host.
//
// Device, Runtime, and Supervisor are operator paths to the isolation
// device and the two processes that supervise a microVM. Empty values use
// platform defaults. Those defaults live in the probe, not in this type.
type Config struct {
	Images      []Image
	Slots       int
	WorkDir     string
	Device      string
	Runtime     string
	Supervisor  string
	CPUMillis   int
	MemoryBytes int64
}

// Host occupies grants as isolated microVMs.
type Host struct {
	mu     sync.Mutex
	cfg    Config
	images map[execenv.Image]Image
	slots  int
	probe  func() error
	launch launcher
	grants map[execenv.ID]*environment
}

// New returns an isolated host. Construction does not require the isolation
// device to be present; Ready and Ensure re-check and fail closed if it is
// gone. That lets a daemon start and report unusable instead of crashing.
func New(cfg Config) (*Host, error) {
	if cfg.WorkDir == "" {
		return nil, execenv.Error("isolated", execenv.ErrInvalid)
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, execenv.Error("isolated", err)
	}
	slots := cfg.Slots
	if slots <= 0 {
		slots = 1
	}
	images := make(map[execenv.Image]Image, len(cfg.Images))
	for _, image := range cfg.Images {
		if image.ID == "" {
			return nil, execenv.Error("isolated", execenv.ErrInvalid)
		}
		images[image.ID] = image
	}
	h := &Host{
		cfg:    cfg,
		images: images,
		slots:  slots,
		probe:  func() error { return probePlatform(cfg) },
		launch: newProcessLauncher(cfg),
		grants: make(map[execenv.ID]*environment),
	}
	return h, nil
}

// Capabilities reports that this adapter isolates grants.
func (h *Host) Capabilities() execenv.Capabilities {
	return execenv.Capabilities{Isolated: true, Freeze: true}
}

// Ready fails closed (Usable=false) when isolation is missing. Images is
// only the ids whose kernel and rootfs are on disk and whose Hash matches.
// One bad file does not hide the rest of the catalog.
func (h *Host) Ready(ctx context.Context) (execenv.Report, error) {
	if err := ctx.Err(); err != nil {
		return execenv.Report{}, execenv.Error("ready", err)
	}
	usable := h.probe() == nil
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := h.presentIDs()
	free := h.slots - len(h.grants)
	if free < 0 {
		free = 0
	}
	return execenv.Report{Usable: usable, Images: ids, Slots: free}, nil
}

// Ensure starts a microVM for a new grant or returns the existing occupancy.
func (h *Host) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	if err := execenv.ValidateSpec(spec); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	if err := h.probe(); err != nil {
		return nil, execenv.Error("ensure", execenv.ErrUnavailable)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Re-check the files at Ensure time. Do not fetch or repair them.
	image, ok := h.verified(spec.Image)
	if !ok {
		return nil, execenv.Error("ensure", execenv.ErrUnknownImage)
	}
	if env, exists := h.grants[spec.ID]; exists {
		if env.image.ID != spec.Image || env.network != spec.Network {
			return nil, execenv.Error("ensure", execenv.ErrConflict)
		}
		return env, nil
	}
	if len(h.grants) >= h.slots {
		return nil, execenv.Error("ensure", execenv.ErrCapacity)
	}
	treeDir := filepath.Join(h.cfg.WorkDir, "grants", string(spec.ID), workspaceName)
	if err := os.MkdirAll(treeDir, 0o700); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	inst, err := h.launch.Start(ctx, startRequest{
		ID:      spec.ID,
		Kernel:  image.Kernel,
		Rootfs:  image.Rootfs,
		TreeDir: treeDir,
		Network: spec.Network,
		Memory:  h.cfg.MemoryBytes,
		CPU:     h.cfg.CPUMillis,
	})
	if err != nil {
		return nil, execenv.Error("ensure", err)
	}
	env := &environment{
		id:      spec.ID,
		image:   image,
		network: spec.Network,
		host:    h,
		inst:    inst,
		treeDir: treeDir,
		files:   make(map[string]node),
	}
	h.grants[spec.ID] = env
	return env, nil
}

// Revoke stops the microVM and frees the slot.
func (h *Host) Revoke(ctx context.Context, id execenv.ID) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("revoke", err)
	}
	h.mu.Lock()
	env, ok := h.grants[id]
	if ok {
		delete(h.grants, id)
	}
	h.mu.Unlock()
	if !ok {
		return nil
	}
	return env.revoke()
}

func (h *Host) grant(id execenv.ID) (*environment, error) {
	h.mu.Lock()
	env, ok := h.grants[id]
	h.mu.Unlock()
	if !ok {
		return nil, execenv.Error("guest", execenv.ErrRevoked)
	}
	return env, nil
}
