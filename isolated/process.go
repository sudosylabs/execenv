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
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
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
	cmd  *exec.Cmd
	sock string
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
		conn, err := dialGuest(ctx, sock)
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

func dialGuest(ctx context.Context, sock string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agent.LinkPort); err != nil {
		_ = conn.Close()
		return nil, err
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "OK\n" {
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
	var release []func() error
	fail := func(err error) (instance, error) {
		for i := len(release) - 1; i >= 0; i-- {
			_ = release[i]()
		}
		return nil, err
	}
	uid, err := l.uids.acquire()
	if err != nil {
		return nil, err
	}
	release = append(release, func() error { l.uids.release(uid); return nil })
	// The guest formats nothing: the host creates the empty writable disk
	// and the guest mounts it. The host never mounts this disk, because its
	// blocks are written by the guest kernel.
	if err := createWorkspaceDisk(ctx, mkfs, req.Disk, req.DiskBytes); err != nil {
		return fail(err)
	}
	jail := newJailLayout(l.jailBase(), runtimeBin, req.ID, uid, uid)
	release = append(release, jail.remove)
	if err := jail.populate(req); err != nil {
		return fail(err)
	}
	// Attach is already prepared by occupancy. None means no NIC. The tap
	// was created by root; the jailed monitor must be allowed to open it.
	if req.Attach != nil {
		if err := setTapOwner(req.Attach.Dev, uid); err != nil {
			return fail(execenv.ErrNetwork)
		}
	}
	vm, err := writeMachineConfig(req, jail)
	if err != nil {
		return fail(err)
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
	}
	args = append(args, "--", "--config-file", vm)
	cmd := exec.Command(supervisor, args...)
	cmd.Dir = filepath.Dir(req.TreeDir)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	configureChild(cmd)
	if err := cmd.Start(); err != nil {
		return fail(execenv.ErrUnavailable)
	}
	release = append(release, func() error { removeJailCgroups(filepath.Base(runtimeBin)); return nil })
	if req.Attach != nil {
		attach := req.Attach
		release = append(release, func() error { attach.close(); return nil })
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return &processInstance{cmd: cmd, sock: jail.host(jailLinkSoc), release: release, done: done}, nil
}

// cleanStale reclaims what a crashed daemon left: jailed monitors (which
// the parent-death signal no longer reaches once the supervisor dropped
// privileges), then their jails.
func (l *processLauncher) cleanStale() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	killJailed(l.uids)
	if err := unmountUnder(l.jailBase()); err != nil {
		return err
	}
	if runtimeBin, err := lookBinary(l.cfg.Runtime, defaultRuntime); err == nil {
		removeJailCgroups(filepath.Base(runtimeBin))
	}
	return os.RemoveAll(l.jailBase())
}

func (p *processInstance) Pause(ctx context.Context) error {
	return signalProcess(ctx, p.cmd, true)
}

func (p *processInstance) Resume(ctx context.Context) error {
	return signalProcess(ctx, p.cmd, false)
}

func (p *processInstance) Stop(ctx context.Context) error {
	if p.cmd == nil || p.cmd.Process == nil || p.done == nil {
		return execenv.ErrUnavailable
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	// The launch-owned waiter reaps exactly once. Killing a process is not a
	// destruction acknowledgement until that waiter observes its exit.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
	}
	var errs []error
	for len(p.release) > 0 {
		last := len(p.release) - 1
		if err := p.release[last](); err != nil {
			errs = append(errs, err)
			break
		}
		p.release = p.release[:last]
	}
	return errors.Join(errs...)
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
	mem := req.Memory / (1024 * 1024)
	if mem <= 0 {
		mem = 128
	}
	vcpus := req.CPU / 1000
	if vcpus <= 0 {
		vcpus = 1
	}
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
			"boot_args":         bootArgs(req.Attach),
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
			"iface_id":      "eth0",
			"guest_mac":     req.Attach.MAC,
			"host_dev_name": req.Attach.Dev,
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

func bootArgs(attach *netAttach) string {
	args := "console=ttyS0 reboot=k panic=1 pci=off init=" + execenv.GuestInit
	if attach == nil {
		return args
	}
	return args + " ip=" + attach.GuestIP + "::" + attach.HostIP + ":255.255.255.252::eth0:off"
}
