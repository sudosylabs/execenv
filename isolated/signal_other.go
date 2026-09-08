//go:build !linux

package isolated

import (
	"context"
	"github.com/sudosylabs/execenv"
	"os/exec"
)

func signalProcess(context.Context, *exec.Cmd, bool) error { return execenv.ErrUnavailable }
