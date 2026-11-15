//go:build !windows

package ctl

import (
	"path/filepath"
	"syscall"
)

// diskFree is the space available under dir, or under its nearest existing
// parent before bootstrap creates it.
func diskFree(dir string) (int64, bool) {
	for dir != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err == nil {
			return int64(st.Bavail) * int64(st.Bsize), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return 0, false
}
