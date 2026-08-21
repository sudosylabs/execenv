//go:build !linux

package isolated

import "os/exec"

func configureChild(_ *exec.Cmd) {}
