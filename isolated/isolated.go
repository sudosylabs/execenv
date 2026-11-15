// Package isolated is an execenv host that occupies each grant in its own
// microVM. Exported names describe isolation, not a particular hypervisor.
package isolated

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/capacity"
	"github.com/sudosylabs/execenv/internal/control"
	"github.com/sudosylabs/execenv/internal/lifetime"
	"github.com/sudosylabs/execenv/internal/procs"
	"github.com/sudosylabs/execenv/internal/seed"
)

var (
	_ execenv.Host = (*Host)(nil)
	_ execenv.Env  = (*environment)(nil)
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

// Config constructs an isolated host. Each grant runs in a microVM, and the
// host fails closed without usable virtualization hardware. A guest whose
// helper cannot serve the native journal is refused.
//
// Device, Runtime, and Supervisor are operator paths to the virtualization
// device, the machine monitor, and its jail supervisor. Empty values use
// platform defaults. Those defaults live in the probe, not in this type.
type Config struct {
	Images     []Image
	WorkDir    string
	Device     string
	Runtime    string
	Supervisor string
	// Capacity is the host's totals. CPUMillis already includes the
	// operator's overcommit; memory is never overcommitted. Defaults is
	// what a Spec's zero Resources fields mean, and GrantMax the most one
	// grant may ask for (zero fields: the totals). Each grant's sparse
	// writable workspace disk is its DiskBytes; it holds the projected tree
	// and the shell's scratch home and is removed on revoke and on
	// stale-grant cleanup.
	Capacity execenv.Resources
	Defaults execenv.Resources
	GrantMax execenv.Resources
	// Mkfs is the ext4 formatter used for workspace disks. Empty uses
	// mkfs.ext4 from PATH.
	Mkfs string
	// JailUIDBase is the first of 65536 host uids (and gids) handed to
	// jailed microVM monitors, one per live grant. Zero uses 2000000000.
	// Nothing else on the host may use the range: a restarted host kills
	// every process running under it.
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
	pool    *capacity.Pool
	seeds   *seed.Store
	probe   func() error
	launch  launcher
	attach  attacher
	grants  map[execenv.ID]*environment
	pending map[execenv.ID]*ensureCall
	// lost holds each grant whose environment ended without a revoke until
	// the caller revokes it, so Ensure cannot silently start it afresh.
	lost      map[execenv.ID]*environment
	lostCount int
	draining  bool
	lock      *os.File
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
	if _, err := parseAllow(cfg.Allow); err != nil {
		return nil, execenv.Error("isolated", execenv.ErrInvalid)
	}
	pool, err := capacity.New(cfg.Capacity, cfg.Defaults, cfg.GrantMax)
	if err != nil {
		return nil, execenv.Error("isolated", err)
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
	probe, launch, attach := func() error { return probePlatform(cfg) }, launcher(newProcessLauncher(cfg)), attacher(allowAttacher{})
	// Stale cleanup runs under the work directory lock, before the grants
	// directory is cleared, so leftovers of a crashed daemon are released
	// (jailed monitors, mounts, grant links) rather than merely unlinked.
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
		pool:    pool,
		probe:   probe,
		launch:  launch,
		attach:  attach,
		grants:  make(map[execenv.ID]*environment),
		pending: make(map[execenv.ID]*ensureCall),
		lost:    make(map[execenv.ID]*environment),
		lock:    lock,
	}
	seeds, err := seed.Open(filepath.Join(cfg.WorkDir, "seeds"))
	if err != nil {
		_ = lock.Close()
		return nil, execenv.Error("isolated", err)
	}
	// The launcher is read when a seed is put, not now, so the one serving
	// grants is the one that builds their seeds.
	seeds.Prepare = func(stage string) error {
		if p, ok := h.launch.(seedPreparer); ok {
			return p.prepareSeed(stage)
		}
		return nil
	}
	h.seeds = seeds
	return h, nil
}

// Capabilities reports that this adapter isolates grants.
func (h *Host) Capabilities() execenv.Capabilities {
	return execenv.Capabilities{Isolated: true}
}

// SetDraining starts or stops draining. A draining host keeps serving the
// grants it holds, including Ensure of one of them, refuses new grants with
// execenv.ErrDraining, and reports Usable false with
// execenv.ReasonDraining. It is operator control for the daemon, not part
// of the execenv.Host contract.
func (h *Host) SetDraining(on bool) {
	h.mu.Lock()
	h.draining = on
	h.mu.Unlock()
}

// Ready fails closed (Usable=false) when isolation is missing or the host
// is draining. Images is only the ids whose kernel and rootfs are on disk
// and whose Hash matches. One bad file does not hide the rest of the
// catalog.
func (h *Host) Ready(ctx context.Context) (execenv.Report, error) {
	if err := ctx.Err(); err != nil {
		return execenv.Report{}, execenv.Error("ready", err)
	}
	usable := h.probe() == nil
	reason := ""
	if !usable {
		reason = execenv.ReasonUnavailable
	}
	h.mu.Lock()
	images := h.snapshotImages()
	held := len(h.pending)
	for _, env := range h.grants {
		if !env.isLost() {
			held++
		}
	}
	lost, draining := h.lostCount, h.draining
	h.mu.Unlock()
	if usable && draining {
		usable, reason = false, execenv.ReasonDraining
	}
	// Hash files outside the lock so a large or odd rootfs cannot stall
	// every other grant on this host.
	networks := []execenv.Network{execenv.NetworkNone}
	if len(h.cfg.Allow) > 0 {
		networks = append(networks, execenv.NetworkAllowlist)
	}
	report := execenv.Report{Usable: usable, Reason: reason, Images: verifiedIDs(images), Networks: networks, Grants: held, Lost: lost, Release: execenv.Release, Isolation: execenv.IsolationVM}
	h.pool.Fill(&report)
	return report, nil
}

// Ensure starts a microVM for a new grant or returns the existing occupancy.
func (h *Host) Ensure(ctx context.Context, spec execenv.Spec) (execenv.Env, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	if err := execenv.ValidateSpec(spec); err != nil {
		return nil, execenv.Error("ensure", err)
	}
	spec.Resources = h.pool.Resolve(spec.Resources)
	if err := h.probe(); err != nil {
		return nil, execenv.Error("ensure", execenv.ErrUnavailable)
	}
	h.mu.Lock()
	if env, call, err := h.occupiedLocked(spec); env != nil || call != nil || err != nil {
		h.mu.Unlock()
		if call != nil {
			return waitEnsure(ctx, call)
		}
		if err != nil {
			return nil, execenv.Error("ensure", err)
		}
		return env.public, nil
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
	if env, call, err := h.occupiedLocked(spec); env != nil || call != nil || err != nil {
		h.mu.Unlock()
		if call != nil {
			return waitEnsure(ctx, call)
		}
		if err != nil {
			return nil, execenv.Error("ensure", err)
		}
		return env.public, nil
	}
	if spec.Network == execenv.NetworkAllowlist && len(h.cfg.Allow) == 0 {
		h.mu.Unlock()
		return nil, execenv.Error("ensure", execenv.ErrNetwork)
	}
	seedDir := ""
	if spec.Seed != "" {
		if _, err := h.seeds.Get(spec.Seed); err != nil {
			h.mu.Unlock()
			return nil, execenv.Error("ensure", err)
		}
		seedDir = h.seeds.Dir(spec.Seed)
	}
	if err := h.pool.Reserve(spec.ID, spec.Resources); err != nil {
		h.mu.Unlock()
		return nil, execenv.Error("ensure", err)
	}
	launchCtx, cancel := context.WithCancel(ctx)
	call := &ensureCall{spec: spec, done: make(chan struct{}), cancel: cancel}
	h.pending[spec.ID] = call
	treeDir := filepath.Join(h.cfg.WorkDir, "grants", string(spec.ID), workspaceName)
	if err := os.MkdirAll(treeDir, 0o700); err != nil {
		delete(h.pending, spec.ID)
		h.pool.Release(spec.ID)
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
		Seed:      seedDir,
		Resources: spec.Resources,
		Limits:    spec.Limits,
		Attach:    att,
	})
	if err != nil {
		if att != nil {
			att.close()
		}
		return h.completeEnsure(call, nil, execenv.Error("ensure", err), treeDir)
	}
	env := &environment{
		gone:    make(chan struct{}),
		control: control.New(spec.ID),
		id:      spec.ID,
		spec:    spec,
		image:   image,
		host:    h,
		inst:    inst,
		treeDir: treeDir,
	}
	env.public = env
	env.procs = procs.New(env.startProcess)
	if err := env.dial(launchCtx); err != nil {
		_ = env.revoke()
		return h.completeEnsure(call, nil, execenv.Error("ensure", err), treeDir)
	}
	if spec.WallTimeout > 0 || spec.IdleTimeout > 0 {
		env.timer = lifetime.Start(spec.WallTimeout, spec.IdleTimeout, func(r lifetime.Reason) { h.expire(env, r) })
		env.public = lifetime.Watch(env, env.timer)
	}
	return h.completeEnsure(call, env, nil, treeDir)
}

// expire applies a grant's wall or idle timeout.
func (h *Host) expire(env *environment, r lifetime.Reason) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if r == lifetime.Idle && env.spec.IdleAction == execenv.IdleFreeze {
		_, _ = env.control.Idle(ctx, func() error { return env.freeze(ctx) })
		return
	}
	_ = h.revokeExact(ctx, env)
}

// occupiedLocked resolves an Ensure that needs no new machine: the grant is
// held (env), starting (call), lost, conflicting, or refused because the
// host is draining (err). All nil means the caller may start one.
func (h *Host) occupiedLocked(spec execenv.Spec) (*environment, *ensureCall, error) {
	if _, lost := h.lost[spec.ID]; lost {
		return nil, nil, execenv.ErrLost
	}
	if env, exists := h.grants[spec.ID]; exists {
		if !execenv.SameGrant(env.spec, spec) {
			return nil, nil, execenv.ErrConflict
		}
		return env, nil, nil
	}
	if call, exists := h.pending[spec.ID]; exists {
		if !execenv.SameGrant(call.spec, spec) {
			return nil, nil, execenv.ErrConflict
		}
		return nil, call, nil
	}
	if h.draining {
		return nil, nil, execenv.ErrDraining
	}
	return nil, nil, nil
}

func waitEnsure(ctx context.Context, call *ensureCall) (execenv.Env, error) {
	select {
	case <-ctx.Done():
		return nil, execenv.Error("ensure", ctx.Err())
	case <-call.done:
		if call.err != nil {
			return nil, call.err
		}
		return call.env.public, nil
	}
}

func (h *Host) completeEnsure(call *ensureCall, env *environment, err error, treeDir string) (execenv.Env, error) {
	h.mu.Lock()
	delete(h.pending, call.spec.ID)
	if err == nil {
		h.grants[call.spec.ID] = env
	} else {
		h.pool.Release(call.spec.ID)
	}
	call.env = env
	call.err = err
	call.cancel()
	close(call.done)
	h.mu.Unlock()
	if err != nil {
		_ = os.RemoveAll(filepath.Dir(treeDir))
		return nil, err
	}
	go env.watch()
	return env.public, nil
}

// Revoke stops the microVM and frees its resources.
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
	// Revoke acknowledges a loss; the id can then be ensured afresh.
	delete(h.lost, id)
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
	if h.lost[env.id] == env {
		delete(h.lost, env.id)
	}
	if h.grants[env.id] != env {
		return nil
	}
	return h.retireLocked(env)
}

// lose retires env because its machine ended without a revoke. It frees
// every host resource the grant held, as a revoke would, and keeps the id
// lost until the caller revokes it. If release fails the resources stay
// reserved, the grant stays lost, and the caller's Revoke retries.
func (h *Host) lose(env *environment) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.grants[env.id] != env {
		return
	}
	lost, err := env.control.Lose(context.Background(), func() error { return env.stop(execenv.ErrLost) })
	if !lost {
		return
	}
	h.lostCount++
	h.lost[env.id] = env
	if err != nil {
		return
	}
	if err := os.RemoveAll(filepath.Join(h.cfg.WorkDir, "grants", string(env.id))); err != nil {
		return
	}
	delete(h.grants, env.id)
	h.pool.Release(env.id)
}

// Keep the resources and identity reserved until destruction is acknowledged. This
// also prevents old cleanup from deleting a newly occupied grant directory.
func (h *Host) retireLocked(env *environment) error {
	if err := env.revoke(); err != nil {
		return execenv.Error("revoke", err)
	}
	if err := os.RemoveAll(filepath.Join(h.cfg.WorkDir, "grants", string(env.id))); err != nil {
		return execenv.Error("revoke", err)
	}
	delete(h.grants, env.id)
	h.pool.Release(env.id)
	return nil
}
