package isolated

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
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
	cfg Config
}

func newProcessLauncher(cfg Config) launcher {
	return &processLauncher{cfg: cfg}
}

type processInstance struct {
	cmd     *exec.Cmd
	dir     string
	cleanup func()
}

func (p *processInstance) Connect(ctx context.Context) (net.Conn, error) {
	// The supervisor exposes the guest link as a Unix socket next to the
	// machine config. CONNECT is the host-side handshake. Retry until
	// ctx ends: the guest helper is not listening the instant Start returns.
	sock := filepath.Join(p.dir, "guest.sock")
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
	if err := os.MkdirAll(req.TreeDir, 0o700); err != nil {
		return nil, err
	}
	// Network none: no guest NIC. Allowlist: a tap plus a host filter
	// of operator dests. Fail closed rather than boot an open NIC.
	var attach *netAttach
	if req.Network == execenv.NetworkAllowlist {
		att, err := setupAllowlist(req.ID, req.Allow)
		if err != nil {
			return nil, err
		}
		attach = att
	}
	vm, err := writeMachineConfig(req, attach)
	if err != nil {
		if attach != nil {
			attach.close()
		}
		return nil, err
	}
	// jailer --id <grant> --exec-file <runtime> --chroot-base-dir <parent>
	// --uid --gid -- --config-file <vm.json>
	// The supervisor chroots the runtime so a breakout from the guest
	// still cannot see other grants.
	grantDir := filepath.Dir(req.TreeDir)
	// workDir/grants/<id>/workspace → chroot base is workDir so one
	// grant cannot see another grant's directory.
	base := l.cfg.WorkDir
	if base == "" {
		base = filepath.Dir(grantDir)
	}
	cmd := exec.Command(supervisor,
		"--id", string(req.ID),
		"--exec-file", runtimeBin,
		"--uid", strconv.Itoa(os.Getuid()),
		"--gid", strconv.Itoa(os.Getgid()),
		"--chroot-base-dir", base,
		"--",
		"--config-file", vm,
	)
	cmd.Dir = grantDir
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		if attach != nil {
			attach.close()
		}
		return nil, execenv.ErrUnavailable
	}
	cleanup := func() {}
	if attach != nil {
		cleanup = attach.close
	}
	return &processInstance{cmd: cmd, dir: grantDir, cleanup: cleanup}, nil
}

func (p *processInstance) Pause() error {
	return signalProcess(p.cmd, true)
}

func (p *processInstance) Resume() error {
	return signalProcess(p.cmd, false)
}

func (p *processInstance) Stop() error {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
	}
	if p.cleanup != nil {
		p.cleanup()
		p.cleanup = nil
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

func writeMachineConfig(req startRequest, attach *netAttach) (string, error) {
	mem := req.Memory / (1024 * 1024)
	if mem <= 0 {
		mem = 128
	}
	vcpus := req.CPU / 1000
	if vcpus <= 0 {
		vcpus = 1
	}
	// Machine JSON follows the supervisor's config-file schema. Rootfs
	// is read-only. The agent writes Home inside the guest; the image
	// must make that directory writable. TreeDir is the stub Home, not
	// a second host drive.
	doc := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": req.Kernel,
			"boot_args":         bootArgs(attach),
		},
		"drives": []map[string]any{
			{
				"drive_id":       "rootfs",
				"path_on_host":   req.Rootfs,
				"is_root_device": true,
				"is_read_only":   true,
			},
		},
		"machine-config": map[string]any{
			"vcpu_count":   vcpus,
			"mem_size_mib": mem,
		},
		"vsock": map[string]any{
			"guest_cid": guestCID(req.ID),
			"uds_path":  filepath.Join(filepath.Dir(req.TreeDir), "guest.sock"),
		},
	}
	if attach != nil {
		doc["network-interfaces"] = []map[string]any{{
			"iface_id":      "eth0",
			"guest_mac":     attach.MAC,
			"host_dev_name": attach.Dev,
		}}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(req.TreeDir), "machine.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func bootArgs(attach *netAttach) string {
	args := "console=ttyS0 reboot=k panic=1 pci=off home=" + execenv.GuestHome + " init=" + execenv.GuestInit
	if attach == nil {
		return args
	}
	return args + " ip=" + attach.GuestIP + "::" + attach.HostIP + ":255.255.255.252::eth0:off"
}
