//go:build linux

package workload

import (
	"errors"
	"os"
	"os/exec"

	"github.com/sudosylabs/execenv"
	"golang.org/x/sys/unix"
)

// RunInit keeps PID 1 separate from the agent and reaps adopted descendants.
// The agent owns its direct shell's wait; detached workload descendants become
// this init's children and cannot accumulate as unreaped zombies after teardown.
//
// Reaping needs PID 1, not root. A sandbox runtime may start this init
// already unprivileged; a contained workload still requires root, which the
// agent checks when it opens the workload hierarchy.
func RunInit(args []string) error {
	if os.Getpid() != 1 {
		return execenv.ErrUnavailable
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, append([]string{"agent"}, args...)...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
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
