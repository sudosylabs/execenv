package isolated

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"os"

	"github.com/sudosylabs/execenv"
)

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

func (h *Host) snapshotImages() []Image {
	out := make([]Image, 0, len(h.images))
	for _, image := range h.images {
		out = append(out, image)
	}
	return out
}

func verifiedIDs(images []Image) []execenv.Image {
	ids := make([]execenv.Image, 0, len(images))
	for _, image := range images {
		if image.matchesCatalog() {
			ids = append(ids, image.ID)
		}
	}
	return ids
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
