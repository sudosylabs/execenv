//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package isolated

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// prepareWorkDir takes the work directory's lock, so one host serves it.
// The grants a previous host left stay for adoption.
func prepareWorkDir(workDir string) (*os.File, error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(workDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(workDir, "grants"), 0o700); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}
