package isolated

import (
	"errors"
	"os/exec"
	"path/filepath"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/hostplatform"
)

// Platform defaults for the isolation device and the two supervisor
// processes. They are not exported so the public isolated API stays
// hypervisor-neutral.
const (
	defaultDevice     = hostplatform.Device
	defaultRuntime    = "firecracker"
	defaultSupervisor = "jailer"
)

// probePlatform fails closed unless this machine can run isolated grants:
// Linux, a usable isolation device, both supervisor processes, and a work
// directory where device nodes work.
func probePlatform(cfg Config) error {
	device := cfg.Device
	if device == "" {
		device = defaultDevice
	}
	if err := hostplatform.CheckDevice(device); err != nil {
		return errors.Join(execenv.ErrUnavailable, err)
	}
	if err := hostplatform.CheckCgroups(); err != nil {
		return errors.Join(execenv.ErrUnavailable, err)
	}
	if err := checkDeviceNodes(cfg.WorkDir); err != nil {
		return errors.Join(execenv.ErrUnavailable, err)
	}
	if _, err := lookBinary(cfg.Runtime, defaultRuntime); err != nil {
		return execenv.ErrUnavailable
	}
	if _, err := lookBinary(cfg.Supervisor, defaultSupervisor); err != nil {
		return execenv.ErrUnavailable
	}
	if _, err := lookBinary(cfg.Mkfs, defaultMkfs); err != nil {
		return execenv.ErrUnavailable
	}
	return nil
}

func lookBinary(configured, fallback string) (string, error) {
	name := configured
	if name == "" {
		name = fallback
	}
	found, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Abs(found)
}
