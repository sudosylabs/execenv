package isolated

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
	"github.com/sudosylabs/execenv/internal/unixsock"
)

// processLauncher starts a supervised microVM. The public isolated types
// never mention the concrete binaries; this file is the only place that
// builds their command lines.
type processLauncher struct {
	cfg  Config
	uids *jailUIDs
}

func newProcessLauncher(cfg Config) launcher {
	return &processLauncher{cfg: cfg, uids: newJailUIDs(cfg.JailUIDBase)}
}

// jailBase is the supervisor's chroot base. Only jails live under it, so a
// restarted host can reclaim it wholesale.
func (l *processLauncher) jailBase() string { return filepath.Join(l.cfg.WorkDir, jailsDir) }

type processInstance struct {
	// pid and start name the monitor for good; it is this host's child
	// only if this host started it.
	pid   int
	start uint64
	rec   machine
	sock  string
	// api is the monitor's API socket, for limits changed while it runs.
	api  string
	done <-chan struct{}
	// release runs in reverse order once the monitor has been reaped. A
	// failed step is retried by the next Stop.
	release []func() error
}

func (p *processInstance) Connect(ctx context.Context) (net.Conn, error) {
	// The supervisor exposes the guest link as a Unix socket next to the
	// machine config, inside the jail. CONNECT is the host-side handshake.
	// Retry until ctx ends: the guest helper is not listening the instant
	// Start returns.
	sock := p.sock
	var last error
	for {
		conn, err := dialGuest(ctx, sock, agent.LinkPort)
		if err == nil {
			return conn, nil
		}
		last = err
		select {
		case <-ctx.Done():
			if last == nil {
				last = ctx.Err()
			}
			return nil, execenv.ErrUnavailable
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// dialGuest connects to port on the guest link.
func dialGuest(ctx context.Context, sock string, port uint32) (net.Conn, error) {
	conn, err := unixsock.Dial(ctx, sock)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// The answer is one line, "OK" and possibly the host-side port. Read it
	// a byte at a time so nothing of the link's first bytes is consumed.
	line := make([]byte, 0, 32)
	one := make([]byte, 1)
	for len(line) < cap(line) {
		if _, err := io.ReadFull(conn, one); err != nil {
			_ = conn.Close()
			return nil, execenv.ErrUnavailable
		}
		if one[0] == '\n' {
			break
		}
		line = append(line, one[0])
	}
	if s := string(line); s != "OK" && !strings.HasPrefix(s, "OK ") {
		_ = conn.Close()
		return nil, execenv.ErrUnavailable
	}
	return conn, nil
}

func (l *processLauncher) Start(ctx context.Context, req startRequest) (instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" {
		return nil, execenv.ErrUnavailable
	}
	runtimeBin, err := lookBinary(l.cfg.Runtime, defaultRuntime)
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	supervisor, err := lookBinary(l.cfg.Supervisor, defaultSupervisor)
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	if req.Kernel == "" || req.Rootfs == "" {
		return nil, execenv.ErrInvalid
	}
	if _, err := os.Stat(req.Kernel); err != nil {
		return nil, execenv.ErrUnknownImage
	}
	if _, err := os.Stat(req.Rootfs); err != nil {
		return nil, execenv.ErrUnknownImage
	}
	mkfs, err := lookBinary(l.cfg.Mkfs, defaultMkfs)
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	if err := os.MkdirAll(req.TreeDir, 0o700); err != nil {
		return nil, err
	}
	p := &processInstance{}
	if req.Attach != nil {
		p.release = append(p.release, func() error { req.Attach.close(); return nil })
	}
	fail := func(err error) (instance, error) {
		if cleanupErr := p.releaseResources(); cleanupErr != nil {
			return p, errors.Join(err, cleanupErr)
		}
		return nil, err
	}
	uid, err := l.uids.acquire()
	if err != nil {
		return nil, err
	}
	p.release = append(p.release, func() error { l.uids.release(uid); return nil })
	// The guest formats nothing: the host creates the empty writable disk
	// and the guest mounts it. The host never mounts this disk, because its
	// blocks are written by the guest kernel.
	// A suspended grant resumes on the disk it had.
	switch {
	case req.Restore != nil && !req.Restore.Start:
	case req.Seed != "":
		if err := cloneSeedDisk(ctx, filepath.Join(req.Seed, seedDiskName), req.Disk, req.Resources.DiskBytes); err != nil {
			return fail(err)
		}
	default:
		if err := createWorkspaceDisk(ctx, mkfs, req.Disk, req.Resources.DiskBytes); err != nil {
			return fail(err)
		}
	}
	jail := newJailLayout(l.jailBase(), runtimeBin, req.ID, uid, uid)
	if err := os.MkdirAll(filepath.Dir(jail.dir), 0o700); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(jail.dir, 0o700); err != nil {
		return fail(err)
	}
	p.release = append(p.release, jail.remove)
	if err := jail.populate(req); err != nil {
		return fail(err)
	}
	if req.Restore != nil {
		if err := jail.placeSaved(req.Restore); err != nil {
			return fail(err)
		}
	}
	// Attach is already prepared by occupancy. None means no NIC. The tap
	// was created by root; the jailed monitor must be allowed to open it.
	if req.Attach != nil {
		if err := setTapOwner(req.Attach.Dev, uid); err != nil {
			return fail(execenv.ErrNetwork)
		}
	}
	// A restored machine takes its configuration from what was saved.
	var vm string
	if req.Restore == nil {
		vm, err = writeMachineConfig(req, jail)
		if err != nil {
			return fail(err)
		}
	}
	// jailer --id <jail-id> --exec-file <runtime> --uid --gid
	// --chroot-base-dir <work>/jails -- --config-file /machine.json
	// The supervisor copies the runtime into the jail, chroots it, and drops
	// it to this grant's own identity before the config is read, so a
	// breakout from the guest lands in an empty root as an unprivileged
	// user that owns no other grant.
	args := []string{
		"--id", jail.id,
		"--exec-file", runtimeBin,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(uid),
		"--chroot-base-dir", l.jailBase(),
	}
	if cgroupV2() {
		args = append(args, "--cgroup-version", "2")
		args = append(args, cgroupLimits(req.Resources)...)
	}
	// The API socket is how UpdateLimits reaches the running machine. It
	// sits in the jail, owned by the grant's identity.
	args = append(args, "--")
	if req.Restore == nil {
		args = append(args, "--config-file", vm)
	}
	args = append(args, "--api-sock", inside(jailAPISock))
	cmd := exec.Command(supervisor, args...)
	cmd.Dir = filepath.Dir(req.TreeDir)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	configureChild(cmd)
	if err := cmd.Start(); err != nil {
		return fail(execenv.ErrUnavailable)
	}
	p.release = append(p.release, func() error { removeJailCgroups(filepath.Base(runtimeBin)); return nil })
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	pid := cmd.Process.Pid
	start, _ := procStart(pid)
	rec := machine{
		PID: pid, Start: start, UID: uid,
		Jail: jail.dir, Exec: filepath.Base(runtimeBin),
		Sock: jail.host(jailLinkSoc), API: jail.host(jailAPISock),
		Net: netRecordOf(req.Attach),
	}
	p.pid, p.start, p.rec, p.sock, p.api, p.done = pid, start, rec, rec.Sock, rec.API, done
	if err := l.bring(ctx, req, p); err != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopErr := p.Stop(stopCtx)
		cancel()
		if stopErr != nil {
			return p, errors.Join(err, stopErr)
		}
		return nil, err
	}
	return p, nil
}

// Adopt takes over a machine a previous host started. It is the same
// machine only if its pid still runs with the start time recorded.
func (l *processLauncher) Adopt(_ context.Context, m machine) (instance, error) {
	if start, ok := procStart(m.PID); !ok || start != m.Start || m.PID <= 0 {
		return nil, execenv.ErrLost
	}
	l.uids.claim(m.UID)
	att, err := m.Net.attach()
	if err != nil {
		return nil, execenv.ErrLost
	}
	release := []func() error{
		func() error { l.uids.release(m.UID); return nil },
		jailLayout{dir: m.Jail}.remove,
		func() error { removeJailCgroups(m.Exec); return nil },
	}
	if att != nil {
		release = append(release, func() error { att.close(); return nil })
	}
	return &processInstance{pid: m.PID, start: m.Start, rec: m, sock: m.Sock, api: m.API, release: release, done: watchExit(m.PID, m.Start)}, nil
}

func (p *processInstance) Machine() machine { return p.rec }

// cleanStale reclaims what a previous daemon left that is not being
// adopted: jailed monitors, their taps, filters and NAT, then their jails.
func (l *processLauncher) cleanStale(keep []machine) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	uids := map[int]bool{}
	jails := map[string]bool{}
	var nets []*netAttach
	for _, m := range keep {
		uids[m.UID] = true
		jails[m.Jail] = true
		if att, err := m.Net.attach(); err == nil && att != nil {
			nets = append(nets, att)
		}
	}
	killJailed(l.uids, uids)
	// The other monitors are gone, so their taps can be deleted.
	cleanStaleLinks(nets)
	execs, err := os.ReadDir(l.jailBase())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range execs {
		parent := filepath.Join(l.jailBase(), e.Name())
		dirs, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, d := range dirs {
			dir := filepath.Join(parent, d.Name())
			if jails[dir] {
				continue
			}
			if err := unmountUnder(dir); err != nil {
				return err
			}
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	if runtimeBin, err := lookBinary(l.cfg.Runtime, defaultRuntime); err == nil {
		removeJailCgroups(filepath.Base(runtimeBin))
	}
	return nil
}

// Done closes when the monitor process has been reaped. The guest kernel
// resets on panic, and a reset ends the monitor, so this also covers a
// crashed guest.
func (p *processInstance) Done() <-chan struct{} { return p.done }

func (p *processInstance) Pause(ctx context.Context) error {
	return signalProcess(ctx, p.pid, true)
}

func (p *processInstance) Resume(ctx context.Context) error {
	return signalProcess(ctx, p.pid, false)
}

func (p *processInstance) Stop(ctx context.Context) error {
	if p.pid == 0 && p.done == nil {
		return p.releaseResources()
	}
	if p.pid <= 0 || p.done == nil {
		return execenv.ErrUnavailable
	}
	select {
	case <-p.done:
	default:
		// Only the recorded machine is killed: a pid whose start time
		// changed is someone else's.
		if start, ok := procStart(p.pid); ok && (p.start == 0 || start == p.start) {
			if proc, err := os.FindProcess(p.pid); err == nil {
				if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					return err
				}
			}
		}
	}
	// Killing a process is not a destruction acknowledgement until its
	// exit has been observed.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
	}
	return p.releaseResources()
}

func (p *processInstance) releaseResources() error {
	for len(p.release) > 0 {
		last := len(p.release) - 1
		if err := p.release[last](); err != nil {
			return err
		}
		p.release = p.release[:last]
	}
	return nil
}

func guestCID(id execenv.ID) uint32 {
	sum := sha256.Sum256([]byte(id))
	cid := binary.BigEndian.Uint32(sum[:4])
	if cid < 3 {
		return cid + 3
	}
	return cid
}

// writeMachineConfig writes the monitor's config into the jail. Every path
// in it is the monitor's view from inside the jail root; the returned path
// is the one to pass after the supervisor's "--".
func writeMachineConfig(req startRequest, jail jailLayout) (string, error) {
	mem := req.Resources.MemoryBytes >> 20
	vcpus := vcpuCount(req.Resources.CPUMillis)
	// Machine JSON follows the supervisor's config-file schema. Rootfs is
	// read-only and shared by every grant of the image. The second drive is
	// this grant's sparse writable disk; the guest init mounts it and
	// places the projected tree and the scratch home on it, so neither
	// consumes guest RAM.
	//
	// The balloon starts deflated. Free page reporting lets the guest hand
	// freed pages back to the host continuously, so a grant's resident
	// memory tracks what it uses rather than its peak. deflate_on_oom lets
	// the guest reclaim balloon pages before invoking its OOM killer. Both
	// need the guest kernel's virtio balloon driver with page reporting;
	// without it the device is inert and the grant behaves as before.
	doc := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": inside(jailKernel),
			"boot_args":         bootArgs(),
		},
		"drives": []map[string]any{
			{
				"drive_id":       "rootfs",
				"path_on_host":   inside(jailRootfs),
				"is_root_device": true,
				"is_read_only":   true,
			},
			{
				"drive_id":       "workspace",
				"path_on_host":   inside(jailDisk),
				"is_root_device": false,
				"is_read_only":   false,
				"rate_limiter":   rateLimiter(req.Limits.DiskBytes, req.Limits.DiskOps),
			},
		},
		"balloon": map[string]any{
			"amount_mib":               0,
			"deflate_on_oom":           true,
			"stats_polling_interval_s": 0,
			"free_page_reporting":      true,
		},
		"machine-config": map[string]any{
			"vcpu_count":   vcpus,
			"mem_size_mib": mem,
		},
		"vsock": map[string]any{
			"guest_cid": guestCID(req.ID),
			"uds_path":  inside(jailLinkSoc),
		},
	}
	if req.Attach != nil {
		doc["network-interfaces"] = []map[string]any{{
			"iface_id":        "eth0",
			"guest_mac":       req.Attach.MAC,
			"host_dev_name":   req.Attach.Dev,
			"rx_rate_limiter": rateLimiter(req.Limits.NetworkBytes, execenv.TokenBucket{}),
			"tx_rate_limiter": rateLimiter(req.Limits.NetworkBytes, execenv.TokenBucket{}),
		}}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	path := jail.host(jailConfig)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	// The monitor reads its config after dropping to the jail identity.
	if err := os.Chown(path, jail.uid, jail.gid); err != nil {
		return "", err
	}
	return inside(jailConfig), nil
}

// bootArgs is the guest kernel's command line. The network, when there is
// one, is configured from the start message, not here, so a booted guest
// and a restored one are set up the same way.
func bootArgs() string {
	return "console=ttyS0 reboot=k panic=1 pci=off init=" + execenv.GuestInit
}
