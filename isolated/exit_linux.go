//go:build linux

package isolated

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// procStart is a process's start time in clock ticks since boot, from
// /proc. With the pid it names one process for good: a pid reused by
// another process has another start time.
func procStart(pid int) (uint64, bool) {
	body, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	// The comm field may contain spaces and parentheses.
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 {
		return 0, false
	}
	fields := strings.Fields(string(body[end+1:]))
	// fields[0] is the state (field 3); the start time is field 22.
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return 0, false
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return start, err == nil
}

// watchExit closes once the process pid that started at start has exited.
// It is how a restarted host learns that an adopted machine, no longer its
// child, has ended.
func watchExit(pid int, start uint64) <-chan struct{} {
	done := make(chan struct{})
	fd, err := unix.PidfdOpen(pid, 0)
	if err == nil {
		// The pid might have been reused between the check and the open.
		if now, ok := procStart(pid); !ok || now != start {
			_ = unix.Close(fd)
			close(done)
			return done
		}
		go func() {
			defer close(done)
			defer unix.Close(fd)
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			for {
				n, err := unix.Poll(fds, -1)
				if err == unix.EINTR {
					continue
				}
				if err != nil || n > 0 {
					return
				}
			}
		}()
		return done
	}
	go func() {
		defer close(done)
		for {
			if now, ok := procStart(pid); !ok || now != start {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	return done
}
