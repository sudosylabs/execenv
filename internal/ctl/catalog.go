package ctl

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/isolated"
)

// Index is the published catalog. Hash is SHA-256 of the shared kernel
// file followed by that image's rootfs. Empty Hash means the id is a
// recipe only and cannot be installed yet.
type Index struct {
	Kernel string       `json:"kernel"`
	Images []IndexImage `json:"images"`
}

// IndexImage is one published disk. Rootfs is the download name.
type IndexImage struct {
	ID     string `json:"id"`
	Rootfs string `json:"rootfs"`
	Hash   string `json:"hash"`
}

func parseIndex(raw []byte) (Index, error) {
	var idx Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return Index{}, wrap("catalog", execenv.ErrInvalid)
	}
	if idx.Kernel == "" {
		idx.Kernel = "vmlinux"
	}
	return idx, nil
}

func loadIndexFile(path string) (Index, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Index{}, wrap("catalog", err)
	}
	return parseIndex(raw)
}

func (idx Index) lookup(id string) (IndexImage, error) {
	for _, img := range idx.Images {
		if img.ID == id {
			if img.Rootfs == "" || !isolated.ValidDigest(img.Hash) {
				return IndexImage{}, wrap("catalog", execenv.ErrInvalid)
			}
			return img, nil
		}
	}
	return IndexImage{}, wrap("catalog", fmt.Errorf("%w: %s", execenv.ErrUnknownImage, id))
}
