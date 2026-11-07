//go:build darwin || linux

package agent

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/workload"
	"golang.org/x/sys/unix"
)

// startShell is a login-style interactive shell whose cwd is the projected
// tree and whose HOME is guest-local scratch outside it, so caches and
// dotfiles written through HOME are not watched or harvested.
func startShell(home, scratch string, win execenv.Window, group *workload.Group) (*shell, error) {
	cmd := exec.Command("/bin/sh", "-l")
	if group != nil {
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cmd = exec.Command(executable, "workload-shell")
		if err := group.Configure(cmd); err != nil {
			return nil, err
		}
	}
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + scratch,
		"PWD=" + home,
		"TERM=xterm-256color",
		"PATH=" + os.Getenv("PATH"),
	}
	master, err := startPTY(cmd, win)
	if err != nil {
		return nil, err
	}
	sh := &shell{master: master, cmd: cmd, exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(sh.exited) }()
	return sh, nil
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
