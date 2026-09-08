//go:build darwin || linux

package agent

import (
	"os/exec"
	"syscall"
	"time"
)

// hangup is PTY close: SIGHUP the login shell. Kill only if it ignores
// hangup so Close cannot leave a stuck guest process.
func hangup(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGHUP)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}
