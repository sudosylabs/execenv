//go:build !linux

package isolated

import "github.com/sudosylabs/execenv"

func setupAllowlist(execenv.ID, []string) (*netAttach, error) {
	return nil, execenv.ErrUnavailable
}

func setupSandboxAllowlist(execenv.ID, []string) (*netAttach, error) {
	return nil, execenv.ErrUnavailable
}

func cleanSandboxNetworks() {}

func cleanStaleLinks() {}
