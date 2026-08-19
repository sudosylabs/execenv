package ctl

import (
	"errors"
	"os"

	"github.com/sudosylabs/execenv/daemon"
)

func loadHost(path string) (daemon.Config, error) {
	cfg, err := daemon.Load(path)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return daemon.Config{}, os.ErrNotExist
	}
	return cfg, err
}

func upsertImage(cfg *daemon.Config, img daemon.Image) {
	for i := range cfg.Images {
		if cfg.Images[i].ID == img.ID {
			cfg.Images[i] = img
			return
		}
	}
	cfg.Images = append(cfg.Images, img)
}
