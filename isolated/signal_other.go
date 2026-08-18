//go:build !unix

package isolated

import "os/exec"

func signalProcess(*exec.Cmd, bool) error { return nil }
