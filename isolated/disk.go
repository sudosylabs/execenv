package isolated

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/sudosylabs/execenv"
)

const (
	// defaultDiskBytes is the per-grant workspace disk when the operator
	// sets none. The file is sparse: only written blocks use host disk.
	defaultDiskBytes = 2 << 30
	// minDiskBytes keeps mkfs from producing a filesystem too small to
	// hold its own metadata plus a useful tree.
	minDiskBytes = 64 << 20
	defaultMkfs  = "mkfs.ext4"
	// diskName is the grant-relative file that backs the writable disk.
	diskName = "workspace.ext4"
	// diskLabel names the filesystem so a guest or host can identify it.
	diskLabel = "execenv-ws"
	// The disk's top level holds the projected tree and the scratch home.
	diskWorkspace = "workspace"
	diskHome      = "home"
)

func diskBytes(cfg Config) int64 {
	if cfg.DiskBytes <= 0 {
		return defaultDiskBytes
	}
	return cfg.DiskBytes
}

// createWorkspaceDisk makes a sparse ext4 image at path. Inode tables and
// the journal are left uninitialised because a fresh sparse file already
// reads as zeros; mounting with noinit_itable keeps the kernel from
// zeroing them later and allocating the whole table.
func createWorkspaceDisk(ctx context.Context, mkfs, path string, size int64) error {
	if size < minDiskBytes {
		return execenv.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	cmd := exec.CommandContext(ctx, mkfs, mkfsArgs(path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Run(); err != nil {
		_ = os.Remove(path)
		return execenv.ErrUnavailable
	}
	return nil
}

func mkfsArgs(path string) []string {
	return []string{
		"-q", "-F",
		"-L", diskLabel,
		"-m", "0",
		"-E", "lazy_itable_init=1,lazy_journal_init=1,nodiscard",
		path,
	}
}
