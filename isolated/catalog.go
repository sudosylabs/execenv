package isolated

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"os"

	"github.com/sudosylabs/execenv"
)

// SumRootfs returns the lowercase hex SHA-256 of the file at path.
// Operators put this value in Image.Hash so Ready can advertise the image.
func SumRootfs(path string) (string, error) {
	sum, err := hashFile(path)
	if err != nil {
		return "", execenv.Error("catalog", err)
	}
	return sum, nil
}

// present reports whether kernel and rootfs exist on disk and the rootfs
// matches Hash. A missing or corrupt artifact is false, never a panic.
// Empty Hash is treated as unverified and therefore absent.
func (img Image) present() bool {
	if img.Hash == "" || img.Kernel == "" || img.Rootfs == "" {
		return false
	}
	if !regularFile(img.Kernel) {
		return false
	}
	sum, err := hashFile(img.Rootfs)
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(img.Hash)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got, err := hex.DecodeString(sum)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(want, got) == 1
}

func (h *Host) presentIDs() []execenv.Image {
	ids := make([]execenv.Image, 0, len(h.images))
	for id, image := range h.images {
		if image.present() {
			ids = append(ids, id)
		}
	}
	return ids
}

func (h *Host) verified(id execenv.Image) (Image, bool) {
	image, ok := h.images[id]
	if !ok || !image.present() {
		return Image{}, false
	}
	return image, true
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
