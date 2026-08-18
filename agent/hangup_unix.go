//go:build darwin || linux

package agent

import (
	"os/exec"
	"syscall"
	"time"
)

// hangup is PTY close: SIGHUP the login shell. Kill only if it ignores
// hangup so Close cannot leave a stuck guest process.
func hangup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}
