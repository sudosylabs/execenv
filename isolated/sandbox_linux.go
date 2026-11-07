//go:build linux

package isolated

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sudosylabs/execenv"
	"golang.org/x/sys/unix"
)

// probeSandbox fails closed unless this machine can run user-kernel
// grants: root (the runtime, loop mounts and namespaces need it), the
// runtime binary answering its version query, the ext4 formatter, and the
// mount helper that attaches loop devices.
func probeSandbox(cfg Config) error {
	if os.Geteuid() != 0 {
		return execenv.ErrUnavailable
	}
	bin, err := lookBinary(cfg.Runtime, defaultSandboxRuntime)
	if err != nil {
		return execenv.ErrUnavailable
	}
	if err := exec.Command(bin, "--version").Run(); err != nil {
		return execenv.ErrUnavailable
	}
	if _, err := lookBinary(cfg.Mkfs, defaultMkfs); err != nil {
		return execenv.ErrUnavailable
	}
	if _, err := exec.LookPath("mount"); err != nil {
		return execenv.ErrUnavailable
	}
	return nil
}

// mountImageRoot attaches a catalog rootfs read-only on the host. noload
// skips journal replay so the image file is never written.
func mountImageRoot(rootfs, dir string) error {
	return runMount("-t", "ext4", "-o", "loop,ro,noload,nodev,nosuid", rootfs, dir)
}

// mountWorkspaceDisk attaches a grant's disk and prepares its two trees,
// owned by the sandbox's unprivileged user. Unlike a microVM's disk, the
// host kernel parses this filesystem, but only the host created and wrote
// it: sandboxed processes reach it through file operations, never blocks.
func mountWorkspaceDisk(disk, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := runMount("-t", "ext4", "-o", "loop,noatime,nodev,nosuid,noinit_itable", disk, dir); err != nil {
		return err
	}
	for _, name := range []string{diskWorkspace, diskHome} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			_ = unmountPath(dir)
			return err
		}
		if err := os.Lchown(path, sandboxUID, sandboxUID); err != nil {
			_ = unmountPath(dir)
			return err
		}
	}
	return nil
}

func runMount(args ...string) error {
	cmd := exec.Command("mount", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Run(); err != nil {
		return execenv.ErrUnavailable
	}
	return nil
}

// unmountPath detaches a mount, retrying briefly while the runtime's last
// file references drain. A loop device set up by mount is released with
// it. A path that is not mounted is not an error.
func unmountPath(dir string) error {
	var err error
	for range 50 {
		err = unix.Unmount(dir, 0)
		if err == nil || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Still busy: detach lazily so the grant directory can be reclaimed
	// and report the failure so the slot is not reused silently.
	_ = unix.Unmount(dir, unix.MNT_DETACH)
	return err
}

// unmountUnder detaches every mount below root, deepest first. It runs
// at construction to release a crashed daemon's loop mounts before the
// grants directory is removed; removing a mounted tree would delete files
// through the mount and then fail.
func unmountUnder(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()
	var points []string
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 5 {
			continue
		}
		point := unescapeMount(fields[4])
		if strings.HasPrefix(point, root+string(filepath.Separator)) {
			points = append(points, point)
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	sort.Slice(points, func(i, j int) bool { return len(points[i]) > len(points[j]) })
	for _, point := range points {
		if err := unix.Unmount(point, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	return nil
}

// unescapeMount decodes the octal escapes mountinfo uses for spaces,
// tabs, newlines and backslashes.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
