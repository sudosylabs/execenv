//go:build darwin || linux

package agent

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/sudosylabs/execenv"
	"golang.org/x/sys/unix"
)

func startPTY(cmd *exec.Cmd, win execenv.Window) (*os.File, error) {
	master, pts, err := openPTY()
	if err != nil {
		return nil, err
	}
	if err := setWindow(master, win); err != nil {
		_ = pts.Close()
		_ = master.Close()
		return nil, err
	}
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	// stdin/stdout/stderr are already the slave. Darwin requires Ctty
	// to be that inherited fd number in the child (0), not the parent's
	// possibly-high descriptor.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = 0
	if err := cmd.Start(); err != nil {
		_ = pts.Close()
		_ = master.Close()
		return nil, err
	}
	_ = pts.Close()
	return master, nil
}

func setWindow(master *os.File, win execenv.Window) error {
	if win.Cols == 0 {
		win.Cols = 80
	}
	if win.Rows == 0 {
		win.Rows = 24
	}
	raw, err := master.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	err = raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{
			Row: win.Rows,
			Col: win.Cols,
		})
	})
	if err != nil {
		return err
	}
	return ioctlErr
}
