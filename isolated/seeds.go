package isolated

import (
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/content"
)

// A seed is kept as a complete workspace disk: the seed's tree under the
// disk's workspace directory, with the empty home and content directories
// beside it, exactly as guest init expects. A grant created with the seed
// gets a copy of that disk, grown to its own size. On a filesystem that
// shares blocks the copy is a reflink; elsewhere the kernel copies only the
// blocks the image uses.
const (
	seedDiskDir  = "disk"
	seedDiskName = "disk.ext4"
	defaultGrow  = "resize2fs"
)

// prepareSeed turns a freshly unpacked seed into its disk image.
func (l *processLauncher) prepareSeed(stage string) error {
	disk := filepath.Join(stage, seedDiskDir)
	if err := os.Mkdir(disk, 0o755); err != nil {
		return content.DiskError(err)
	}
	if err := os.Rename(filepath.Join(stage, "tree"), filepath.Join(disk, diskWorkspace)); err != nil {
		return err
	}
	for _, dir := range []string{diskHome, "content"} {
		if err := os.Mkdir(filepath.Join(disk, dir), 0o755); err != nil {
			return content.DiskError(err)
		}
	}
	size, err := treeBytes(disk)
	if err != nil {
		return err
	}
	mkfs, err := lookBinary(l.cfg.Mkfs, defaultMkfs)
	if err != nil {
		return execenv.ErrUnavailable
	}
	image := filepath.Join(stage, seedDiskName)
	args := append(mkfsArgs(image)[:len(mkfsArgs(image))-1], "-d", disk, image, strconv.FormatInt(seedImageBytes(size)>>10, 10)+"k")
	cmd := exec.Command(mkfs, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Run(); err != nil {
		return execenv.ErrUnavailable
	}
	// The image now holds the tree; the unpacked copy is no longer needed.
	return os.RemoveAll(disk)
}

// seedImageBytes leaves room for the filesystem's own metadata, rounded up
// to a mebibyte. Grants grow the image to their own disk size.
func seedImageBytes(data int64) int64 {
	size := data + data/5 + minDiskBytes
	return (size + 1<<20 - 1) &^ (1<<20 - 1)
}

func treeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Every entry costs at least a block.
		total += max(info.Size(), 4096)
		return nil
	})
	return total, err
}

// cloneSeedDisk makes path a copy of the seed disk image, sized size.
func cloneSeedDisk(ctx context.Context, image, path string, size int64) error {
	info, err := os.Stat(image)
	if err != nil {
		return execenv.ErrNotFound
	}
	if info.Size() > size {
		return execenv.ErrQuota
	}
	if err := copyFile(image, path); err != nil {
		return content.DiskError(err)
	}
	if err := os.Truncate(path, size); err != nil {
		_ = os.Remove(path)
		return content.DiskError(err)
	}
	grow, err := lookBinary("", defaultGrow)
	if err != nil {
		_ = os.Remove(path)
		return execenv.ErrUnavailable
	}
	cmd := exec.CommandContext(ctx, grow, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Run(); err != nil {
		_ = os.Remove(path)
		return execenv.ErrUnavailable
	}
	return nil
}

// copyFile copies src to a new dst. Between two regular files io.Copy uses
// copy_file_range, which reflinks where the filesystem can.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
	}
	return err
}

// PutSeed unpacks a seed and builds its disk image.
func (h *Host) PutSeed(ctx context.Context, digest string, archive io.Reader) (execenv.Seed, error) {
	if err := ctx.Err(); err != nil {
		return execenv.Seed{}, execenv.Error("put_seed", err)
	}
	seed, err := h.seeds.Put(digest, archive)
	if err != nil {
		return execenv.Seed{}, execenv.Error("put_seed", err)
	}
	return seed, nil
}

func (h *Host) Seeds(ctx context.Context) ([]execenv.Seed, error) {
	if err := ctx.Err(); err != nil {
		return nil, execenv.Error("seeds", err)
	}
	return h.seeds.List()
}

func (h *Host) DeleteSeed(ctx context.Context, digest string) error {
	if err := ctx.Err(); err != nil {
		return execenv.Error("delete_seed", err)
	}
	return h.seeds.Delete(digest)
}
