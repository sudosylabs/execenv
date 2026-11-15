//go:build linux

package workload

import (
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/guestlink"
	"github.com/sudosylabs/execenv/internal/start"
	"golang.org/x/sys/unix"
)

// The guest's writable disk and where its parts are bound.
const (
	diskDevice   = "/dev/vdb"
	diskMount    = "/run/execenv/disk"
	homeMount    = "/run/execenv/home"
	machineIDRun = "/run/execenv/machine-id"
)

// The random device's ioctls: add entropy and credit it, then reseed the
// kernel's generator from it.
const (
	rndAddEntropy = 0x40085203
	rndReseedCRNG = 0x5207
)

// RunInit is PID 1's second stage. It waits for the host's start message,
// so a machine restored from a template is this grant's before anything
// runs: it reseeds the random generator, sets the clock and the machine
// id, and only then mounts the grant's workspace disk and starts the
// helper. It then keeps PID 1 separate from the helper and reaps adopted
// descendants, so detached workload processes cannot accumulate as
// zombies.
//
// Reaping needs PID 1, not root. A contained workload still requires root,
// which the helper checks when it opens the workload hierarchy.
func RunInit(args []string) error {
	if os.Getpid() != 1 {
		return execenv.ErrUnavailable
	}
	ln, err := guestlink.Listen(start.Port)
	if err != nil {
		return err
	}
	msg, conn, err := start.Wait(ln)
	_ = ln.Close()
	if err != nil {
		return err
	}
	if err := applyStart(msg); err != nil {
		_ = conn.Close()
		return err
	}
	if err := mountWorkspace(); err != nil {
		_ = conn.Close()
		return err
	}
	_ = start.Acknowledge(conn)
	_ = conn.Close()

	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, append([]string{"agent"}, args...)...)
	command.Dir = execenv.GuestWorkspace
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The helper runs ahead of the workload: it must read a process's
	// command line between its exec and its exit, and a short command on
	// one virtual CPU otherwise ends first. Every thread it starts inherits
	// this from the thread that forks it; workload processes drop it again
	// in Exec.
	runtime.LockOSThread()
	_ = unix.Setpriority(unix.PRIO_PROCESS, 0, helperNice)
	err = command.Start()
	runtime.UnlockOSThread()
	if err != nil {
		return err
	}
	defer command.Process.Release()
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, 0, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return execenv.ErrUnavailable
		}
		if pid == command.Process.Pid {
			if status.Exited() && status.ExitStatus() == 0 {
				return nil
			}
			return execenv.ErrUnavailable
		}
	}
}

// applyStart makes the guest this start's: fresh randomness, the host's
// clock and its own machine id.
func applyStart(m start.Message) error {
	random, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer random.Close()
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	info := make([]byte, 8+len(m.Entropy))
	binary.NativeEndian.PutUint32(info[0:], uint32(len(m.Entropy)*8))
	binary.NativeEndian.PutUint32(info[4:], uint32(len(m.Entropy)))
	copy(info[8:], m.Entropy)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, random.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&info[0]))); errno != 0 {
		return errno
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, random.Fd(), rndReseedCRNG, 0); errno != 0 {
		return errno
	}
	ts := unix.NsecToTimespec(m.Time.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return err
	}
	if err := os.WriteFile(machineIDRun, []byte(m.MachineID+"\n"), 0o444); err != nil {
		return err
	}
	// The image root is read-only and baked without a machine id; one
	// that has the file gets this guest's laid over it.
	if _, err := os.Stat("/etc/machine-id"); err == nil {
		if err := unix.Mount(machineIDRun, "/etc/machine-id", "", unix.MS_BIND, ""); err != nil {
			return err
		}
	}
	return nil
}

// helperNice is the guest helper's scheduling priority, the highest.
const helperNice = -20

// mountWorkspace mounts the grant's writable disk and binds the projected
// tree and the scratch home from it, so neither lives in guest memory.
// Without the disk the guest does not start.
func mountWorkspace() error {
	if err := unix.Mount(diskDevice, diskMount, "ext4", unix.MS_NOATIME|unix.MS_NOSUID|unix.MS_NODEV, "noinit_itable"); err != nil {
		return err
	}
	for _, dir := range []string{"workspace", "home"} {
		if err := os.MkdirAll(filepath.Join(diskMount, dir), 0o755); err != nil {
			return err
		}
	}
	// File bodies the projection and its observations hold live beside
	// the workspace on the same disk, outside what the workload can reach.
	content := filepath.Join(diskMount, "content")
	if err := os.MkdirAll(content, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(content, 0o700); err != nil {
		return err
	}
	if err := unix.Mount(filepath.Join(diskMount, "workspace"), execenv.GuestWorkspace, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	return unix.Mount(filepath.Join(diskMount, "home"), homeMount, "", unix.MS_BIND, "")
}
