//go:build !linux

package agent

import "github.com/sudosylabs/execenv"

// openKernelProcesses needs the Linux process events connector.
func openKernelProcesses(string, string, string) (ProcessSource, error) {
	return nil, execenv.ErrUnavailable
}
