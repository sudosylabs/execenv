//go:build unix

package isolated

import (
	"os/exec"
	"syscall"
)

func signalProcess(cmd *exec.Cmd, pause bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if pause {
		return cmd.Process.Signal(syscall.SIGSTOP)
	}
	return cmd.Process.Signal(syscall.SIGCONT)
}
