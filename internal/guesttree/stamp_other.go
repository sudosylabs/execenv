//go:build !linux && !darwin

package guesttree

import "os"

// stampOf is size and modification time where the change time is not known.
func stampOf(info os.FileInfo) stamp {
	return stamp{size: info.Size(), mtime: info.ModTime().UnixNano()}
}
