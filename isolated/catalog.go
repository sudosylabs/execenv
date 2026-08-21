package isolated

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/sudosylabs/execenv"
)

type fileStamp struct {
	size    int64
	modTime int64
	mode    os.FileMode
}

type imageRecord struct {
	mu      sync.Mutex
	image   Image
	kernel  fileStamp
	rootfs  fileStamp
	checked bool
	valid   bool
}

// ValidDigest reports whether hash is a 64-digit hex SHA-256.
func ValidDigest(hash string) bool {
	raw, err := hex.DecodeString(hash)
	return err == nil && len(raw) == sha256.Size
}

// Digest is the catalog hash: lowercase hex SHA-256 of the kernel file
// followed by the root filesystem file.
func Digest(kernel, rootfs string) (string, error) {
	sum := sha256.New()
	for _, path := range []string{kernel, rootfs} {
		if !regularFile(path) {
			return "", os.ErrNotExist
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(sum, file)
		_ = file.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// matchesCatalog reports whether both boot files are regular files and
// their combined digest equals Hash. A missing or corrupt artifact is
// false, never a panic. I/O happens here so callers must not hold Host.mu.
func (img Image) matchesCatalog() bool {
	if !ValidDigest(img.Hash) || img.Kernel == "" || img.Rootfs == "" {
		return false
	}
	sum, err := Digest(img.Kernel, img.Rootfs)
	if err != nil {
		return false
	}
	want, _ := hex.DecodeString(img.Hash)
	got, err := hex.DecodeString(sum)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(want, got) == 1
}

func newImageRecord(image Image) *imageRecord {
	record := &imageRecord{image: image}
	_, _ = record.verified()
	return record
}

// verified avoids re-hashing multi-gigabyte artifacts on every health poll.
// It revalidates whenever either file's cheap stat fingerprint changes.
func (r *imageRecord) verified() (Image, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kernel, kernelOK := stamp(r.image.Kernel)
	rootfs, rootfsOK := stamp(r.image.Rootfs)
	if r.checked && kernelOK && rootfsOK && kernel == r.kernel && rootfs == r.rootfs {
		return r.image, r.valid
	}
	r.kernel = kernel
	r.rootfs = rootfs
	r.checked = true
	r.valid = kernelOK && rootfsOK && r.image.matchesCatalog()
	return r.image, r.valid

}

func (h *Host) snapshotImages() []*imageRecord {
	out := make([]*imageRecord, 0, len(h.images))
	for _, record := range h.images {
		out = append(out, record)
	}
	return out
}

func verifiedIDs(images []*imageRecord) []execenv.Image {
	ids := make([]execenv.Image, 0, len(images))
	for _, record := range images {
		if image, ok := record.verified(); ok {
			ids = append(ids, image.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func stamp(path string) (fileStamp, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fileStamp{}, false
	}
	return fileStamp{size: info.Size(), modTime: info.ModTime().UnixNano(), mode: info.Mode()}, true
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
