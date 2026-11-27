//go:build unix

package files

import (
	"os"

	"golang.org/x/sys/unix"
)

// A name can change after Lstat. Never block opening a replacement FIFO.
const nonblock = unix.O_NONBLOCK

func renameAt(from *os.File, old string, to *os.File, newName string) error {
	return unix.Renameat(int(from.Fd()), old, int(to.Fd()), newName)
}
