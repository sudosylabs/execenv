package guesttree

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/sudosylabs/execenv"
)

// physicalKey names one physical node without holding it open.
type physicalKey string

// physicalIdentity is the node's file handle, which on ext4 carries the
// inode's generation: a deleted file's inode number reused by a new file
// gets a new handle, so reuse never passes for a move. A filesystem without
// handles falls back to device and inode.
func physicalIdentity(full string, info os.FileInfo) (physicalKey, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode().IsRegular() && stat.Nlink != 1 {
		return "", execenv.ErrLagged
	}
	if handle, _, err := unix.NameToHandleAt(unix.AT_FDCWD, full, 0); err == nil {
		return physicalKey(fmt.Sprintf("h%d:%x", handle.Type(), handle.Bytes())), nil
	}
	return physicalKey(fmt.Sprintf("i%d:%d", stat.Dev, stat.Ino)), nil
}
