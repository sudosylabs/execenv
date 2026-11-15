//go:build !linux && !darwin

package guesttree

import (
	"os"

	"github.com/sudosylabs/execenv"
)

type physicalKey string

// physicalIdentity is unavailable where no guest or test host runs.
func physicalIdentity(string, os.FileInfo) (physicalKey, error) {
	return "", execenv.ErrUnavailable
}
