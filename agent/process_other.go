//go:build !linux

package agent

import "github.com/sudosylabs/execenv"

// openKernelProcesses needs the Linux process events connector.
func openKernelProcesses(string, string, string, func(int) (launch, bool)) (ProcessSource, error) {
	return nil, execenv.ErrUnavailable
}

// runAhead has nothing to raise where there are no kernel process events.
func runAhead() {}
