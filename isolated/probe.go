package isolated

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/sudosylabs/execenv"
)

// Platform defaults for the isolation device and the two supervisor
// processes. They are not exported so the public isolated API stays
// hypervisor-neutral.
const (
	defaultDevice     = "/dev/kvm"
	defaultRuntime    = "firecracker"
	defaultSupervisor = "jailer"
)

// probePlatform fails closed unless this machine can run isolated grants:
// Linux, a usable isolation device, and both supervisor processes.
func probePlatform(cfg Config) error {
	if runtime.GOOS != "linux" {
		return execenv.ErrUnavailable
	}
	device := cfg.Device
	if device == "" {
		device = defaultDevice
	}
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		return execenv.ErrUnavailable
	}
	_ = f.Close()
	if _, err := lookBinary(cfg.Runtime, defaultRuntime); err != nil {
		return execenv.ErrUnavailable
	}
	if _, err := lookBinary(cfg.Supervisor, defaultSupervisor); err != nil {
		return execenv.ErrUnavailable
	}
	return nil
}

func lookBinary(configured, fallback string) (string, error) {
	name := configured
	if name == "" {
		name = fallback
	}
	if filepath.IsAbs(name) || filepath.Dir(name) != "." {
		if _, err := os.Stat(name); err != nil {
			return "", err
		}
		return name, nil
	}
	return exec.LookPath(name)
}
