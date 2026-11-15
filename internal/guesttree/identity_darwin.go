package guesttree

import (
	"fmt"
	"os"
	"syscall"

	"github.com/sudosylabs/execenv"
)

// physicalKey names one physical node without holding it open.
type physicalKey string

// physicalIdentity is device, inode and birth time: an inode number reused
// by a new file has a new birth time. (macOS hosts run tests only.)
func physicalIdentity(_ string, info os.FileInfo) (physicalKey, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode().IsRegular() && stat.Nlink != 1 {
		return "", execenv.ErrLagged
	}
	born := stat.Birthtimespec.Sec*1e9 + stat.Birthtimespec.Nsec
	return physicalKey(fmt.Sprintf("i%d:%d:%d", stat.Dev, stat.Ino, born)), nil
}
