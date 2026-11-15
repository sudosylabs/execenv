//go:build darwin || linux

package agent

import (
	"os/exec"
	"syscall"

	"github.com/sudosylabs/execenv"
)

// signals are the ones a caller may send.
var signals = map[execenv.Signal]syscall.Signal{
	execenv.SignalInterrupt: syscall.SIGINT,
	execenv.SignalTerminate: syscall.SIGTERM,
	execenv.SignalKill:      syscall.SIGKILL,
	execenv.SignalHangup:    syscall.SIGHUP,
	execenv.SignalQuit:      syscall.SIGQUIT,
}

// newSession starts cmd as the leader of its own session.
func newSession(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// signalSession signals cmd's whole session: it leads one, so its process
// group is its pid.
func signalSession(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		_ = cmd.Process.Signal(sig)
	}
}
