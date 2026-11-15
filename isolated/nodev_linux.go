//go:build linux

package isolated

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// checkDeviceNodes fails when the work directory's filesystem ignores device
// nodes, as a tmpfs /tmp mounted nodev does: the supervisor makes the
// isolation device inside each jail, and no machine could open it.
func checkDeviceNodes(workDir string) error {
	dir := workDir
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return nil
	}
	if st.Flags&unix.ST_NODEV != 0 {
		return fmt.Errorf("work directory %s is on a filesystem mounted nodev; grants need device nodes in their jails", workDir)
	}
	return nil
}
