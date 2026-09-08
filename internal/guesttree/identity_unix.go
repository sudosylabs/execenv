//go:build linux || darwin

package guesttree

import (
	"os"
	"syscall"

	"github.com/sudosylabs/execenv"
)

type physicalKey struct{ device, inode uint64 }

func physicalIdentity(info os.FileInfo) (physicalKey, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode().IsRegular() && stat.Nlink != 1 {
		return physicalKey{}, execenv.ErrLagged
	}
	return physicalKey{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}
