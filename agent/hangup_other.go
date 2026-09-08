//go:build !darwin && !linux

package agent

import "os/exec"

func hangup(cmd *exec.Cmd, _ <-chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
