//go:build linux

package workload

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/sudosylabs/execenv"
	"golang.org/x/sys/unix"
)

const uid = 65534

// Group owns the single unprivileged workload in a dedicated guest. Controls
// are root-owned and never delegated to the workload, including its descendants.
type Group struct {
	gate gate
	dir  *os.Root
	fd   *os.File
}

// Open requires the actual unified kernel hierarchy, with the agent outside
// the workload. A regular directory cannot impersonate a working freezer.
func Open() (*Group, error) {
	if os.Geteuid() != 0 {
		return nil, execenv.ErrUnavailable
	}
	root, err := os.OpenRoot("/sys/fs/cgroup")
	if err != nil {
		return nil, execenv.ErrUnavailable
	}
	defer root.Close()
	var stat unix.Statfs_t
	handle, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	if unix.Fstatfs(int(handle.Fd()), &stat) != nil || uint64(stat.Type) != uint64(unix.CGROUP2_SUPER_MAGIC) {
		return nil, execenv.ErrUnavailable
	}
	// Refuse existing groups: after agent reconstruction a previous occupancy
	// must never become usable under fresh in-memory projection/fence state.
	if err := root.Mkdir("execenv-workload", 0700); err != nil {
		return nil, execenv.ErrUnavailable
	}
	dir, err := root.OpenRoot("execenv-workload")
	if err != nil {
		return nil, err
	}
	fd, err := dir.Open(".")
	if err != nil {
		dir.Close()
		return nil, err
	}
	group := &Group{dir: dir, fd: fd}
	group.gate.fs = group
	// Probe real transitions before the workload can start.
	if err := group.gate.stopped(context.Background(), func() error { return nil }); err != nil {
		group.Close()
		return nil, err
	}
	return group, nil
}

func (g *Group) write(name, value string) error   { return g.dir.WriteFile(name, []byte(value), 0600) }
func (g *Group) read(name string) ([]byte, error) { return g.dir.ReadFile(name) }
func (g *Group) Stopped(ctx context.Context, effect func() error) error {
	return g.gate.stopped(ctx, effect)
}

// Configure places the process in its cgroup at creation, before any guest
// instruction executes. There is no post-start migration race.
func (g *Group) Configure(cmd *exec.Cmd) error {
	g.gate.mu.Lock()
	defer g.gate.mu.Unlock()
	if g.gate.failed || g.fd == nil {
		return execenv.ErrUnavailable
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.UseCgroupFD = true
	cmd.SysProcAttr.CgroupFD = int(g.fd.Fd())
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: uid}
	return nil
}

func (g *Group) OwnHome(home string) error { return os.Lchown(home, uid, uid) }

// Kill waits for the whole subtree, including detached descendants. The
// caller must not reuse this occupancy after an uncertain result.
func (g *Group) Kill(ctx context.Context) error {
	g.gate.mu.Lock()
	defer g.gate.mu.Unlock()
	if err := g.write("cgroup.kill", "1"); err != nil {
		g.gate.failed = true
		return execenv.ErrUnavailable
	}
	waitCtx, cancel := context.WithTimeout(ctx, transitionTimeout)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := g.read("cgroup.events")
		if err != nil {
			g.gate.failed = true
			return execenv.ErrUnavailable
		}
		populated, ok := eventValue(raw, "populated")
		if !ok {
			g.gate.failed = true
			return execenv.ErrUnavailable
		}
		if populated == 0 {
			return nil
		}
		select {
		case <-waitCtx.Done():
			g.gate.failed = true
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (g *Group) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), transitionTimeout)
	defer cancel()
	err := g.Kill(ctx)
	g.gate.mu.Lock()
	defer g.gate.mu.Unlock()
	g.gate.failed = true
	if g.fd != nil {
		_ = g.fd.Close()
		g.fd = nil
	}
	_ = g.dir.Close()
	// Keep the empty marker until guest teardown; a new agent may not silently
	// revive the old filesystem with an empty observation/receipt history.
	return err
}

// ExecShell is called only by the internal workload bootstrap command, after
// Configure dropped all identities. Pin this thread through exec so inherited
// no_new_privs also prevents set-ID and file-capability privilege escalation.
func ExecShell() error {
	runtime.LockOSThread()
	if os.Getuid() != uid || os.Geteuid() != uid || os.Getgid() != uid {
		return execenv.ErrUnavailable
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	return syscall.Exec("/bin/sh", []string{"/bin/sh", "-l"}, os.Environ())
}

func (g *Group) Check() error {
	g.gate.mu.Lock()
	defer g.gate.mu.Unlock()
	if g.gate.failed {
		return execenv.ErrUnavailable
	}
	return nil
}
