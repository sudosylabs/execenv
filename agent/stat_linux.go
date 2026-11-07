//go:build linux

package agent

import "syscall"

func statTimes(st *syscall.Stat_t) (mtime, ctime int64) {
	return st.Mtim.Nano(), st.Ctim.Nano()
}
