//go:build !darwin && !linux

package agent

import (
	"os"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/workload"
)

func isPtyHangup(error) bool {
	return false
}

func startShell(string, string, execenv.Window, *workload.Group) (*shell, error) {
	return nil, execenv.ErrUnavailable
}

func setWindow(*os.File, execenv.Window) error {
	return execenv.ErrUnavailable
}
