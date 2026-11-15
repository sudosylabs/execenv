//go:build !darwin && !linux

package agent

import (
	"os/exec"
	"syscall"

	"github.com/sudosylabs/execenv"
)

var signals = map[execenv.Signal]syscall.Signal{
	execenv.SignalKill: syscall.SIGKILL,
}

func newSession(*exec.Cmd) {}

func signalSession(cmd *exec.Cmd, _ syscall.Signal) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
