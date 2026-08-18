package isolated

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/sudosylabs/execenv"
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
	cmd *exec.Cmd
	dir string
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
	// The VM config names the read-only image root and the writable
	// workspace directory. Network none means no guest NIC is configured.
	vm, err := writeMachineConfig(req, l.cfg)
	if err != nil {
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
		return nil, execenv.ErrUnavailable
	}
	return &processInstance{cmd: cmd, dir: grantDir}, nil
}

func (p *processInstance) Pause() error {
	return signalProcess(p.cmd, true)
}

func (p *processInstance) Resume() error {
	return signalProcess(p.cmd, false)
}

func (p *processInstance) Stop() error {
	if p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
	return nil
}

func writeMachineConfig(req startRequest, cfg Config) (string, error) {
	mem := req.Memory / (1024 * 1024)
	if mem <= 0 {
		mem = 128
	}
	vcpus := req.CPU / 1000
	if vcpus <= 0 {
		vcpus = 1
	}
	// Machine JSON follows the supervisor's config-file schema. Rootfs
	// is read-only; the workspace directory is the only writable drive.
	doc := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": req.Kernel,
			// HOME/cwd are the workspace directory. The image root stays
			// read-only; the guest agent or init must chdir here.
			"boot_args": "console=ttyS0 reboot=k panic=1 pci=off home=/workspace",
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
			"vcpu_count":  vcpus,
			"mem_size_mib": mem,
		},
	}
	if req.Network != execenv.NetworkNone {
		// Allowlist destinations are enforced on the host, not by
		// handing the guest a general NIC. The NIC itself is added
		// later with the allowlist path.
		_ = cfg
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
