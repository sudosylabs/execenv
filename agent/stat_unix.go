//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package agent

import (
	"os"
	"syscall"
)

// statOf returns the comparison key for info. A change time at or after
// racy is not trusted: a rewrite in the same timestamp tick could otherwise
// leave size and times unchanged.
func statOf(info os.FileInfo, racy int64) statKey {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return statKey{}
	}
	mtime, ctime := statTimes(st)
	return statKey{
		trusted: ctime < racy && mtime < racy,
		dev:     uint64(st.Dev),
		ino:     uint64(st.Ino),
		size:    st.Size,
		mtime:   mtime,
		ctime:   ctime,
	}
}
