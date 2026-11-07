//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package agent

import "syscall"

func statTimes(st *syscall.Stat_t) (mtime, ctime int64) {
	return st.Mtimespec.Nano(), st.Ctimespec.Nano()
}
