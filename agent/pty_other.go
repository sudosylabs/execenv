//go:build !darwin && !linux

package agent

import (
	"os"

	"github.com/sudosylabs/execenv"
)

func isPtyHangup(error) bool {
	return false
}

func startShell(string, execenv.Window) (*shell, error) {
	return nil, execenv.ErrUnavailable
}

func setWindow(*os.File, execenv.Window) error {
	return execenv.ErrUnavailable
}
