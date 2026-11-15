package guesttree

import (
	"os"
	"syscall"
)

// stampOf is size, modification time and change time. The change time moves
// on every write, even one that restores the modification time.
func stampOf(info os.FileInfo) stamp {
	st := stamp{size: info.Size(), mtime: info.ModTime().UnixNano()}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		st.ctime = sys.Ctimespec.Sec*1e9 + sys.Ctimespec.Nsec
	}
	return st
}
