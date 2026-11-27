//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package ctl

import (
	"os"

	"github.com/sudosylabs/execenv"
)

func lockFile(*os.File) error { return execenv.ErrUnavailable }
