//go:build linux

package isolated

import (
	"testing"

	"github.com/sudosylabs/execenv/execenvtest"
)

func TestListeningPortsConformance(t *testing.T) {
	host := testHost(t, func() error { return nil }, &recordingLauncher{})
	execenvtest.RunListeningPorts(t, host, execenvtest.LocalPortListener(nil))
}
