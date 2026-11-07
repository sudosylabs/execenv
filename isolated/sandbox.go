package isolated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/agent"
)

// The user-kernel boundary runs each grant in an OCI sandbox driven
// directly by its runtime binary: no container engine and no image store.
// The baked catalog rootfs is mounted read-only on the host once per image
// and shared as the bundle root; each grant gets its own sparse workspace
// disk, mounted on the host and bound into the sandbox. The host link is
// the agent's standard input and output.
const (
	defaultSandboxRuntime = "runsc"
	// sandboxUID is the unprivileged identity every sandboxed process runs
	// as. It matches the microVM guest's contained workload user.
	sandboxUID = 65534
	// sandboxMemory is the per-grant memory limit when the operator sets
	// none. It matches the microVM default.
	sandboxMemory = 128 << 20
	// sandboxRunTmp sizes the in-sandbox /run tmpfs that holds mount
	// points for the scratch home. Nothing large is written there.
	sandboxRunTmp = 16 << 20
	sandboxState  = "sandbox-state"
	sandboxImages = "images"
)

type sandboxLauncher struct {
	cfg Config
	mu  sync.Mutex
	// roots are host mounts of catalog rootfs files, shared by grants.
	roots map[string]*rootMount
}

type rootMount struct {
	dir  string
	refs int
}

func newSandboxLauncher(cfg Config) *sandboxLauncher {
	return &sandboxLauncher{cfg: cfg, roots: make(map[string]*rootMount)}
}

// sandboxID is the runtime's container identity for a grant. Grant IDs are
// caller-chosen; the runtime only accepts a restricted alphabet, and the
// hash keeps caller text out of host process listings.
func sandboxID(id execenv.ID) string {
	sum := sha256.Sum256([]byte(id))
	return "execenv-" + hex.EncodeToString(sum[:12])
}

func (l *sandboxLauncher) stateDir() string { return filepath.Join(l.cfg.WorkDir, sandboxState) }

// runtimeArgs are the global flags for every runtime invocation. The state
// root is private to this host so stale cleanup never touches sandboxes
// another tool started.
func (l *sandboxLauncher) runtimeArgs(network bool, sub ...string) []string {
	args := []string{"--root", l.stateDir()}
	if network {
		args = append(args, "--network=sandbox")
	} else {
		args = append(args, "--network=none")
	}
	// The catalog root is read-only and not overlaid; writes go only to the
	// grant's disk, exactly as in a microVM.
	args = append(args, "--overlay2=none")
	return append(args, sub...)
}

func (l *sandboxLauncher) Start(ctx context.Context, req startRequest) (instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" {
		return nil, execenv.ErrUnavailable
	}
	bin, err := lookBinary(l.cfg.Runtime, defaultSandboxRuntime)
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	mkfs, err := lookBinary(l.cfg.Mkfs, defaultMkfs)
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	if req.Rootfs == "" {
		return nil, execenv.ErrInvalid
	}
	if _, err := os.Stat(req.Rootfs); err != nil {
		return nil, execenv.ErrUnknownImage
	}
	grantDir := filepath.Dir(req.TreeDir)
	if err := os.MkdirAll(req.TreeDir, 0o700); err != nil {
		return nil, err
	}
	undo := []func() error{}
	fail := func(err error) (instance, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			_ = undo[i]()
		}
		return nil, err
	}
	if err := createWorkspaceDisk(ctx, mkfs, req.Disk, req.DiskBytes); err != nil {
		return fail(err)
	}
	diskDir := filepath.Join(grantDir, "disk")
	if err := mountWorkspaceDisk(req.Disk, diskDir); err != nil {
		return fail(err)
	}
	undo = append(undo, func() error { return unmountPath(diskDir) })
	root, err := l.acquireRoot(req.Rootfs)
	if err != nil {
		return fail(err)
	}
	undo = append(undo, func() error { return l.releaseRoot(req.Rootfs) })
	bundle := filepath.Join(grantDir, "bundle")
	spec := sandboxSpec(req, root, diskDir)
	if err := writeBundle(bundle, spec); err != nil {
		return fail(err)
	}
	// The agent's stdio is the host link. Standard error is discarded so
	// guest output never reaches host logs.
	hostR, guestW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	guestR, hostW, err := os.Pipe()
	if err != nil {
		_ = hostR.Close()
		_ = guestW.Close()
		return fail(err)
	}
	id := sandboxID(req.ID)
	cmd := exec.Command(bin, l.runtimeArgs(req.Attach != nil, "run", "--bundle", bundle, id)...)
	cmd.Dir = grantDir
	cmd.Stdin = guestR
	cmd.Stdout = guestW
	cmd.Stderr = nil
	configureChild(cmd)
	if err := cmd.Start(); err != nil {
		_ = hostR.Close()
		_ = hostW.Close()
		_ = guestR.Close()
		_ = guestW.Close()
		return fail(execenv.ErrUnavailable)
	}
	_ = guestR.Close()
	_ = guestW.Close()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	inst := &sandboxInstance{
		launcher: l,
		bin:      bin,
		id:       id,
		network:  req.Attach != nil,
		cmd:      cmd,
		done:     done,
		conn:     agent.NewPipeConn(hostR, hostW),
		release:  undo,
	}
	if req.Attach != nil {
		attach := req.Attach
		inst.release = append(inst.release, func() error { attach.close(); return nil })
	}
	return inst, nil
}

func (l *sandboxLauncher) acquireRoot(rootfs string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m := l.roots[rootfs]; m != nil {
		m.refs++
		return m.dir, nil
	}
	sum := sha256.Sum256([]byte(rootfs))
	dir := filepath.Join(l.cfg.WorkDir, sandboxImages, hex.EncodeToString(sum[:8]), "root")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := mountImageRoot(rootfs, dir); err != nil {
		return "", err
	}
	l.roots[rootfs] = &rootMount{dir: dir, refs: 1}
	return dir, nil
}

func (l *sandboxLauncher) releaseRoot(rootfs string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.roots[rootfs]
	if m == nil {
		return nil
	}
	m.refs--
	if m.refs > 0 {
		return nil
	}
	// The mount is forgotten either way: unmountPath detaches lazily when
	// still busy, and the next grant mounts the image afresh.
	delete(l.roots, rootfs)
	return unmountPath(m.dir)
}

// cleanStale releases what a crashed daemon left: sandboxes under this
// host's private state root, then host mounts under the work directory,
// then grant network namespaces.
func (l *sandboxLauncher) cleanStale() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if bin, err := lookBinary(l.cfg.Runtime, defaultSandboxRuntime); err == nil {
		if _, err := os.Stat(l.stateDir()); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			out, _ := exec.CommandContext(ctx, bin, "--root", l.stateDir(), "list", "-quiet").Output()
			for _, id := range bytes.Fields(out) {
				_ = exec.CommandContext(ctx, bin, "--root", l.stateDir(), "delete", "-force", string(id)).Run()
			}
			cancel()
		}
	}
	if err := unmountUnder(l.cfg.WorkDir); err != nil {
		return err
	}
	cleanSandboxNetworks(l.cfg.Allow)
	return os.RemoveAll(filepath.Join(l.cfg.WorkDir, sandboxImages))
}

type sandboxInstance struct {
	launcher *sandboxLauncher
	bin      string
	id       string
	network  bool
	cmd      *exec.Cmd
	done     <-chan struct{}

	mu   sync.Mutex
	conn net.Conn
	// release runs in reverse order after the runtime exits. A failed step
	// is retried by the next Stop; finished steps are not repeated.
	release []func() error
}

// Connect hands out the one stdio link. A sandbox has exactly one; the
// host dials once per occupancy.
func (s *sandboxInstance) Connect(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil, execenv.ErrUnavailable
	}
	conn := s.conn
	s.conn = nil
	return conn, nil
}

func (s *sandboxInstance) runtime(ctx context.Context, sub ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.bin, s.launcher.runtimeArgs(s.network, sub...)...)
	cmd.Stdin, cmd.Stderr = nil, nil
	return cmd.Output()
}

// Pause stops every task in the sandbox. It is acknowledged only after the
// runtime reports the sandbox paused, not when the request was sent.
func (s *sandboxInstance) Pause(ctx context.Context) error {
	return s.transition(ctx, "pause", "paused")
}

func (s *sandboxInstance) Resume(ctx context.Context) error {
	return s.transition(ctx, "resume", "running")
}

func (s *sandboxInstance) transition(ctx context.Context, verb, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.runtime(ctx, verb, s.id); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return execenv.ErrUnavailable
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		raw, err := s.runtime(ctx, "state", s.id)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return execenv.ErrUnavailable
		}
		status, err := sandboxStatus(raw)
		if err != nil {
			return execenv.ErrUnavailable
		}
		if status == want {
			return nil
		}
		if status == "stopped" {
			return execenv.ErrUnavailable
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func sandboxStatus(raw []byte) (string, error) {
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || state.Status == "" {
		return "", execenv.ErrUnavailable
	}
	return state.Status, nil
}

// Stop destroys the sandbox, waits for the runtime to exit, and only then
// releases mounts and the network. Destruction is not acknowledged until
// the supervising runtime process has been reaped.
func (s *sandboxInstance) Stop(ctx context.Context) error {
	if s.cmd == nil || s.cmd.Process == nil || s.done == nil {
		return execenv.ErrUnavailable
	}
	_, _ = s.runtime(ctx, "kill", "-all", s.id, "KILL")
	_, _ = s.runtime(ctx, "delete", "-force", s.id)
	if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	var errs []error
	for len(s.release) > 0 {
		last := len(s.release) - 1
		if err := s.release[last](); err != nil {
			errs = append(errs, err)
			break
		}
		s.release = s.release[:last]
	}
	return errors.Join(errs...)
}

func writeBundle(dir string, spec map[string]any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600)
}

// sandboxSpec is the OCI runtime spec for one grant. The process is the
// same guest init a microVM boots, run unprivileged with no capabilities
// and no_new_privs. CPU and memory limits are cgroup limits on the host.
func sandboxSpec(req startRequest, root, diskDir string) map[string]any {
	memory := req.Memory
	if memory <= 0 {
		memory = sandboxMemory
	}
	cpus := req.CPU
	if cpus <= 0 {
		cpus = 1000
	}
	const period = 100000
	namespaces := []map[string]any{
		{"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"},
	}
	if req.Attach != nil && req.Attach.NetNS != "" {
		namespaces = append(namespaces, map[string]any{"type": "network", "path": req.Attach.NetNS})
	} else {
		namespaces = append(namespaces, map[string]any{"type": "network"})
	}
	return map[string]any{
		"ociVersion": "1.0.2",
		"hostname":   "execenv",
		"root":       map[string]any{"path": root, "readonly": true},
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": sandboxUID, "gid": sandboxUID},
			"cwd":      execenv.GuestWorkspace,
			"args": []string{
				execenv.GuestBin, "guest-init",
				"-home", execenv.GuestWorkspace,
				"-scratch", execenv.GuestHome,
				"-listen", "stdio",
			},
			"env": []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"HOME=" + execenv.GuestHome,
			},
			"noNewPrivileges": true,
			"capabilities": map[string]any{
				"bounding": []string{}, "effective": []string{}, "inheritable": []string{},
				"permitted": []string{}, "ambient": []string{},
			},
			"rlimits": []map[string]any{{"type": "RLIMIT_NOFILE", "hard": 4096, "soft": 4096}},
		},
		"mounts": []map[string]any{
			{"destination": "/proc", "type": "proc", "source": "proc"},
			{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{"destination": "/dev/shm", "type": "tmpfs", "source": "shm", "options": []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{"destination": "/sys", "type": "sysfs", "source": "sysfs", "options": []string{"nosuid", "noexec", "nodev", "ro"}},
			{"destination": "/run", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "nodev", "mode=755", "size=" + strconv.Itoa(sandboxRunTmp)}},
			{"destination": execenv.GuestWorkspace, "type": "bind", "source": filepath.Join(diskDir, diskWorkspace), "options": []string{"rbind", "rw", "nosuid", "nodev"}},
			{"destination": execenv.GuestHome, "type": "bind", "source": filepath.Join(diskDir, diskHome), "options": []string{"rbind", "rw", "nosuid", "nodev"}},
		},
		"linux": map[string]any{
			"namespaces":  namespaces,
			"cgroupsPath": "/execenv/" + sandboxID(req.ID),
			"resources": map[string]any{
				"memory": map[string]any{"limit": memory, "swap": memory},
				"cpu":    map[string]any{"quota": int64(cpus) * period / 1000, "period": period},
				"pids":   map[string]any{"limit": 1024},
			},
		},
	}
}
