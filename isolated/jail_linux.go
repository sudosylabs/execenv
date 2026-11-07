//go:build linux

package isolated

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindReadOnly makes dst a read-only view of src. Two steps: a bind mount
// ignores MS_RDONLY until it is remounted.
func bindReadOnly(src, dst string) error {
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if err := unix.Mount("", dst, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		_ = unix.Unmount(dst, unix.MNT_DETACH)
		return err
	}
	return nil
}

// setTapOwner lets the jailed, unprivileged monitor open a persistent tap
// the host created as root.
func setTapOwner(dev string, uid int) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(dev)
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return err
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETOWNER, uid); err != nil {
		return err
	}
	return unix.IoctlSetInt(fd, unix.TUNSETGROUP, uid)
}

// cgroupV2 reports a unified hierarchy, which the supervisor must be told
// about explicitly (its default is v1).
func cgroupV2() bool {
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	return err == nil
}

// killJailed kills every process whose real uid is in the jail range. The
// supervisor changes credentials before exec, which clears the parent-death
// signal, so a monitor can outlive a crashed daemon; this is how a restarted
// host reclaims it.
func killJailed(pool *jailUIDs) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		uid, ok := procUID(filepath.Join("/proc", entry.Name(), "status"))
		if ok && pool.contains(uid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

func procUID(status string) (int, bool) {
	f, err := os.Open(status)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		uid, err := strconv.Atoi(fields[1])
		return uid, err == nil
	}
	return 0, false
}

// removeJailCgroups removes the empty per-jail cgroups the supervisor may
// have created under its exec-file-named parent.
func removeJailCgroups(execName string) {
	for _, root := range []string{"/sys/fs/cgroup", "/sys/fs/cgroup/cpu", "/sys/fs/cgroup/memory", "/sys/fs/cgroup/pids"} {
		parent := filepath.Join(root, execName)
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "g") && len(entry.Name()) == 25 {
				_ = unix.Rmdir(filepath.Join(parent, entry.Name()))
			}
		}
	}
}
