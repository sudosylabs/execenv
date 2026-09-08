//go:build !linux && !darwin

package guesttree

import (
	"github.com/sudosylabs/execenv"
	"os"
)

type physicalKey struct{ device, inode uint64 }

func physicalIdentity(os.FileInfo) (physicalKey, error) { return physicalKey{}, execenv.ErrUnavailable }
