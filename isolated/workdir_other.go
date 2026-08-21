//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package isolated

import (
	"os"
	"path/filepath"
)

func prepareWorkDir(workDir string) (*os.File, error) {
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(workDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	grants := filepath.Join(workDir, "grants")
	if err := os.RemoveAll(grants); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := os.MkdirAll(grants, 0o700); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}
