//go:build darwin || linux

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/sudosylabs/execenv"
	"golang.org/x/sys/unix"
)

// startShell is a login-style interactive shell whose cwd and HOME are
// the projected tree. A login profile that honors HOME therefore cannot
// walk out of the workspace.
func startShell(home string, win execenv.Window) (*shell, error) {
	cmd := exec.Command("/bin/sh", "-l")
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"PWD=" + home,
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
	}
	master, err := startPTY(cmd, win)
	if err != nil {
		return nil, err
	}
	return &shell{master: master, cmd: cmd}, nil
}

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
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	if err := cmd.Start(); err != nil {
		_ = pts.Close()
		_ = master.Close()
		return nil, err
	}
	_ = pts.Close()
	return master, nil
}

func isPtyHangup(err error) bool {
	return errors.Is(err, unix.EIO)
}

func setWindow(master *os.File, win execenv.Window) error {
	if win.Cols == 0 {
		win.Cols = 80
	}
	if win.Rows == 0 {
		win.Rows = 24
	}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: win.Rows,
		Col: win.Cols,
	})
}
