//go:build linux

package isolated

import (
	"os/exec"
	"syscall"
)

// configureChild makes a directly-run host fail closed on an abrupt daemon
// death. The installed systemd unit also keeps descendants in its cgroup.
func configureChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
