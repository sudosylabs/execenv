// Package isolated is an execenv host that occupies each grant in its own
// microVM. Exported names describe isolation, not a particular hypervisor.
package isolated

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/control"
)

var (
	_ execenv.Host        = (*Host)(nil)
	_ execenv.Env         = (*environment)(nil)
	_ execenv.GuestWriter = (*Host)(nil)
)

// Image is one catalog entry. Kernel and Rootfs are local paths to the
// read-only boot artifacts. Hash is the lowercase hex SHA-256 of the
// kernel file followed by the rootfs file. Images are never pulled.
type Image struct {
	ID     execenv.Image
	Kernel string
	Rootfs string
	Hash   string
}

// Config constructs an isolated host.
//
// Isolation selects the boundary. IsolationVM (the default) runs each grant
// in a microVM and fails closed without usable virtualization hardware.
// IsolationUserKernel runs each grant in a user-space-kernel sandbox; it is
// an explicit operator choice and never a fallback for a VM host.
//
// Device, Runtime, and Supervisor are operator paths to the isolation
// device and the processes that supervise a grant. For IsolationVM these
// are the virtualization device, the machine monitor, and its jail
// supervisor. For IsolationUserKernel only Runtime is used: the sandbox
// runtime. Empty values use platform defaults. Those defaults live in the
// probe, not in this type.
type Config struct {
	Isolation execenv.Isolation
	// RequireJournal refuses guests that cannot prove native journal support.
	// Leave disabled until the installed guest artifacts have been certified.
	RequireJournal bool
	Images         []Image
	Slots          int
	WorkDir        string
	Device         string
	Runtime        string
	Supervisor     string
	CPUMillis      int
	MemoryBytes    int64
	// DiskBytes sizes each grant's sparse writable workspace disk. Zero
	// uses 2 GiB. The disk holds the projected tree and the shell's scratch
	// home; it is removed on revoke and on stale-grant cleanup.
	DiskBytes int64
	// Mkfs is the ext4 formatter used for workspace disks. Empty uses
	// mkfs.ext4 from PATH.
	Mkfs string
	// JailUIDBase is the first of 65536 host uids (and gids) handed to
	// jailed microVM monitors, one per live grant. Zero uses 2000000000.
	// Nothing else on the host may use the range: a restarted host kills
	// every process running under it. Unused by IsolationUserKernel.
	JailUIDBase int
	// Allow is the host's IPv4 dests. Grants cannot add dests. Empty
	// means this host does not offer NetworkAllowlist.
	Allow []string
}

// Host occupies grants as isolated microVMs.
type Host struct {
	mu      sync.Mutex
	cfg     Config
	images  map[execenv.Image]*imageRecord
	slots   int
	probe   func() error
	launch  launcher
	attach  attacher
	grants  map[execenv.ID]*environment
	pending map[execenv.ID]*ensureCall
	lock    *os.File
}

type ensureCall struct {
	spec   execenv.Spec
	done   chan struct{}
	cancel context.CancelFunc
	env    *environment
	err    error
}

// New returns an isolated host. Construction does not require the isolation
// device to be present; Ready and Ensure re-check and fail closed if it is
// gone. That lets a daemon start and report unusable instead of crashing.
func New(cfg Config) (*Host, error) {
	if cfg.WorkDir == "" {
		return nil, execenv.Error("isolated", execenv.ErrInvalid)
	}
	slots := cfg.Slots
	if slots <= 0 {
		slots = 1
	}
	if _, err := parseAllow(cfg.Allow); err != nil {
		return nil, execenv.Error("isolated", execenv.ErrInvalid)
	}
	switch cfg.Isolation {
	case "":
		cfg.Isolation = execenv.IsolationVM
	case execenv.IsolationVM:
	case execenv.IsolationUserKernel:
		// The native journal relies on the guest's own kernel workload
		// barrier, which this boundary does not certify.
		if cfg.RequireJournal {
			return nil, execenv.Error("isolated", execenv.ErrInvalid)
		}
	default:
		return nil, execenv.Error("isolated", execenv.ErrInvalid)
	}
	if cfg.DiskBytes != 0 && cfg.DiskBytes < minDiskBytes {
		return nil, execenv.Error("isolated", execenv.ErrInvalid)
	}
	images := make(map[execenv.Image]*imageRecord, len(cfg.Images))
	for _, image := range cfg.Images {
		if image.ID == "" || image.Kernel == "" || image.Rootfs == "" || !ValidDigest(image.Hash) {
			return nil, execenv.Error("isolated", execenv.ErrInvalid)
		}
		if _, dup := images[image.ID]; dup {
			return nil, execenv.Error("isolated", execenv.ErrInvalid)
		}
		images[image.ID] = newImageRecord(image)
	}
	probe, launch, attach := func() error { return probePlatform(cfg) }, newProcessLauncher(cfg), attacher(allowAttacher{})
	if cfg.Isolation == execenv.IsolationUserKernel {
		sandbox := newSandboxLauncher(cfg)
		probe, launch, attach = func() error { return probeSandbox(cfg) }, sandbox, sandboxAttacher{}
	}
	// Stale cleanup runs under the work directory lock, before the grants
	// directory is cleared, so leftovers of a crashed daemon are released
	// (sandboxes, mounts, namespaces) rather than merely unlinked.
	var stale func() error
	if cleaner, ok := launch.(staleCleaner); ok {
		stale = cleaner.cleanStale
	}
	lock, err := prepareWorkDir(cfg.WorkDir, stale)
	if err != nil {
		return nil, execenv.Error("isolated", err)
	}
	h := &Host{
		cfg:     cfg,
		images:  images,
		slots:   slots,
		probe:   probe,
		launch:  launch,
		attach:  attach,
		grants:  make(map[execenv.ID]*environment),
		pending: make(map[execenv.ID]*ensureCall),
		lock:    lock,
	}
	return h, nil
}

// Capabilities reports that this adapter isolates grants.
func (h *Host) Capabilities() execenv.Capabilities {
	return execenv.Capabilities{Isolated: true, Freeze: true, OrderedControl: true, JournalProjection: h.cfg.RequireJournal, SemanticObservations: h.cfg.RequireJournal}
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
	images := h.snapshotImages()
	free := h.slots - len(h.grants) - len(h.pending)
	h.mu.Unlock()
	if free < 0 {
		free = 0
	}
	// Hash files outside the lock so a large or odd rootfs cannot stall
	// every other grant on this host.
	networks := []execenv.Network{execenv.NetworkNone}
	if len(h.cfg.Allow) > 0 {
		networks = append(networks, execenv.NetworkAllowlist)
	}
	return execenv.Report{Usable: usable, Images: verifiedIDs(images), Networks: networks, Slots: free, Release: execenv.Release, Isolation: h.cfg.Isolation}, nil
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
	if env, exists := h.grants[spec.ID]; exists {
		conflict := env.image.ID != spec.Image || env.network != spec.Network
		h.mu.Unlock()
		if conflict {
			return nil, execenv.Error("ensure", execenv.ErrConflict)
		}
		return env, nil
	}
	if call, exists := h.pending[spec.ID]; exists {
		if call.spec.Image != spec.Image || call.spec.Network != spec.Network {
			h.mu.Unlock()
			return nil, execenv.Error("ensure", execenv.ErrConflict)
		}
		h.mu.Unlock()
		return waitEnsure(ctx, call)
	}
	record, ok := h.images[spec.Image]
	h.mu.Unlock()
	// Unverified or missing files are reported as unknown so callers
	// cannot tell a typo from a corrupt disk, and no host path leaks.
	if !ok {
		return nil, execenv.Error("ensure", execenv.ErrUnknownImage)
	}
	image, verified := record.verified()
	if !verified {
		return nil, execenv.Error("ensure", execenv.ErrUnknownImage)
	}
	h.mu.Lock()
	// Catalog verification happens without Host.mu. Recheck occupancy before
	// reserving because another caller may have completed in the meantime.
	if env, exists := h.grants[spec.ID]; exists {
		conflict := env.image.ID != spec.Image || env.network != spec.Network
		h.mu.Unlock()
		if conflict {
			return nil, execenv.Error("ensure", execenv.ErrConflict)
		}
		return env, nil
	}
	if call, exists := h.pending[spec.ID]; exists {
		if call.spec.Image != spec.Image || call.spec.Network != spec.Network {
			h.mu.Unlock()
			return nil, execenv.Error("ensure", execenv.ErrConflict)
		}
		h.mu.Unlock()
		return waitEnsure(ctx, call)
	}
	if spec.Network == execenv.NetworkAllowlist && len(h.cfg.Allow) == 0 {
		h.mu.Unlock()
		return nil, execenv.Error("ensure", execenv.ErrNetwork)
	}
	if len(h.grants)+len(h.pending) >= h.slots {
		h.mu.Unlock()
		return nil, execenv.Error("ensure", execenv.ErrCapacity)
	}
	launchCtx, cancel := context.WithCancel(ctx)
	call := &ensureCall{spec: spec, done: make(chan struct{}), cancel: cancel}
	h.pending[spec.ID] = call
	treeDir := filepath.Join(h.cfg.WorkDir, "grants", string(spec.ID), workspaceName)
	if err := os.MkdirAll(treeDir, 0o700); err != nil {
		delete(h.pending, spec.ID)
		cancel()
		h.mu.Unlock()
		return nil, execenv.Error("ensure", err)
	}
	dests := append([]string(nil), h.cfg.Allow...)
	h.mu.Unlock()
	var att *netAttach
	if spec.Network == execenv.NetworkAllowlist {
		var err error
		att, err = h.attach.Setup(spec.ID, dests)
		if err != nil {
			return h.completeEnsure(call, nil, execenv.Error("ensure", err), treeDir)
		}
	}
	inst, err := h.launch.Start(launchCtx, startRequest{
		ID:        spec.ID,
		Kernel:    image.Kernel,
		Rootfs:    image.Rootfs,
		TreeDir:   treeDir,
		Disk:      filepath.Join(filepath.Dir(treeDir), diskName),
		DiskBytes: diskBytes(h.cfg),
		Attach:    att,
		Memory:    h.cfg.MemoryBytes,
		CPU:       h.cfg.CPUMillis,
	})
	if err != nil {
		if att != nil {
			att.close()
		}
		return h.completeEnsure(call, nil, execenv.Error("ensure", err), treeDir)
	}
	env := &environment{
		control: control.New(spec.ID),
		id:      spec.ID,
		image:   image,
		network: spec.Network,
		host:    h,
		inst:    inst,
		treeDir: treeDir,
	}
	if err := env.dial(launchCtx); err != nil {
		_ = env.revoke()
		return h.completeEnsure(call, nil, execenv.Error("ensure", err), treeDir)
	}
	return h.completeEnsure(call, env, nil, treeDir)
}

func waitEnsure(ctx context.Context, call *ensureCall) (execenv.Env, error) {
	select {
	case <-ctx.Done():
		return nil, execenv.Error("ensure", ctx.Err())
	case <-call.done:
		return call.env, call.err
	}
}

func (h *Host) completeEnsure(call *ensureCall, env *environment, err error, treeDir string) (execenv.Env, error) {
	h.mu.Lock()
	delete(h.pending, call.spec.ID)
	if err == nil {
		h.grants[call.spec.ID] = env
	}
	call.env = env
	call.err = err
	call.cancel()
	close(call.done)
	h.mu.Unlock()
	if err != nil {
		_ = os.RemoveAll(filepath.Dir(treeDir))
	}
	return env, err
}

// Revoke stops the microVM and frees the slot.
func (h *Host) Revoke(ctx context.Context, id execenv.ID) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("revoke", err)
	}
	for {
		h.mu.Lock()
		call := h.pending[id]
		if call == nil {
			break
		}
		call.cancel()
		done := call.done
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return execenv.Error("revoke", ctx.Err())
		case <-done:
		}
	}
	defer h.mu.Unlock()
	env := h.grants[id]
	if env == nil {
		return nil
	}
	return h.retireLocked(env)
}

func (h *Host) revokeExact(ctx context.Context, env *environment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants[env.id] != env {
		return nil
	}
	return h.retireLocked(env)
}

// Keep the slot and identity reserved until destruction is acknowledged. This
// also prevents old cleanup from deleting a newly occupied grant directory.
func (h *Host) retireLocked(env *environment) error {
	if err := env.revoke(); err != nil {
		return execenv.Error("revoke", err)
	}
	if err := os.RemoveAll(filepath.Join(h.cfg.WorkDir, "grants", string(env.id))); err != nil {
		return execenv.Error("revoke", err)
	}
	delete(h.grants, env.id)
	return nil
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
