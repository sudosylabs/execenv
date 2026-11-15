//go:build linux

package isolated

import (
	"os/exec"
	"syscall"
)

// configureChild starts a machine in its own session, with no parent-death
// signal: a grant's machine outlives the daemon, and the next daemon adopts
// it from the grant's state file.
func configureChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
