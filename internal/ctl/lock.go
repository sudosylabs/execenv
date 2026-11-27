package ctl

import (
	"errors"
	"os"
	"path/filepath"
)

type configLock struct {
	file *os.File
}

func (l configLock) Close() error {
	// Release the lock even if another descriptor still refers to the file.
	return errors.Join(unlockFile(l.file), l.file.Close())
}

// lockConfig serializes commands that publish host configuration and its
// files. The kernel drops the lock if the manager terminates.
func (o Options) lockConfig() (*configLock, error) {
	if err := os.MkdirAll(o.Sysconf, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(o.Sysconf, ".manager.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &configLock{file: file}, nil
}
