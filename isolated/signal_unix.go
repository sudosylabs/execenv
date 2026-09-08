//go:build linux

package isolated

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sudosylabs/execenv"
)

// SIGSTOP applies to the entire thread group, including every execution
// thread. A successful signal send is not a suspension acknowledgement.
func signalProcess(ctx context.Context, cmd *exec.Cmd, pause bool) error {
	if cmd == nil || cmd.Process == nil {
		return execenv.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sig := syscall.SIGCONT
	if pause {
		sig = syscall.SIGSTOP
	}
	if err := cmd.Process.Signal(sig); err != nil {
		return err
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		confirmed, err := processState(cmd.Process.Pid, pause)
		if err != nil {
			return execenv.ErrUnavailable
		}
		if confirmed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func processState(pid int, stopped bool) (bool, error) {
	dir := fmt.Sprintf("/proc/%d/task", pid)
	threads, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	if len(threads) == 0 {
		return false, execenv.ErrUnavailable
	}
	for _, thread := range threads {
		body, err := os.ReadFile(filepath.Join(dir, thread.Name(), "stat"))
		if err != nil {
			return false, err
		}
		// The comm field may contain spaces and parentheses.
		end := strings.LastIndexByte(string(body), ')')
		if end < 0 || end+2 >= len(body) {
			return false, execenv.ErrUnavailable
		}
		state := body[end+2]
		if state == 'Z' || state == 'X' {
			return false, execenv.ErrUnavailable
		}
		if (state == 'T' || state == 't') != stopped {
			return false, nil
		}
	}
	return true, nil
}
