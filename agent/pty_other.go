//go:build !darwin && !linux

package agent

import (
	"os"
	"os/exec"

	"github.com/sudosylabs/execenv"
)

func isPtyHangup(error) bool {
	return false
}

func startPTY(*exec.Cmd, execenv.Window) (*os.File, error) {
	return nil, execenv.ErrUnavailable
}

func setWindow(*os.File, execenv.Window) error {
	return execenv.ErrUnavailable
}
