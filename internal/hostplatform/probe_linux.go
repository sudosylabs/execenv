//go:build linux

// Package hostplatform checks Linux prerequisites shared by bootstrap and
// the isolated host.
package hostplatform

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	Device = "/dev/kvm"

	kvmGetAPIVersion = 0xae00
	kvmCreateVM      = 0xae01
	kvmAPIVersion    = 12
)

// CheckDevice verifies the device the monitor will open and proves that it
// can create a VM. A configured override must be an alias of that device.
func CheckDevice(path string) error {
	var configured, monitor unix.Stat_t
	if err := unix.Stat(path, &configured); err != nil {
		return err
	}
	if configured.Mode&unix.S_IFMT != unix.S_IFCHR {
		return fmt.Errorf("%s is not a character device", path)
	}
	fd, err := unix.Open(Device, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Fstat(fd, &monitor); err != nil {
		return err
	}
	if monitor.Mode&unix.S_IFMT != unix.S_IFCHR || configured.Rdev != monitor.Rdev {
		return fmt.Errorf("%s does not identify %s", path, Device)
	}
	version, err := unix.IoctlRetInt(fd, kvmGetAPIVersion)
	if err != nil {
		return err
	}
	if version != kvmAPIVersion {
		return fmt.Errorf("unsupported KVM API version %d", version)
	}
	vm, err := unix.IoctlRetInt(fd, kvmCreateVM)
	if err != nil {
		return fmt.Errorf("create KVM VM: %w", err)
	}
	return unix.Close(vm)
}

// CheckCgroups verifies the controller hierarchy needed for grant limits.
func CheckCgroups() error {
	data, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return fmt.Errorf("cgroup v2 is required: %w", err)
	}
	controllers := strings.Fields(string(data))
	if !slices.Contains(controllers, "cpu") || !slices.Contains(controllers, "memory") {
		return fmt.Errorf("cgroup v2 CPU and memory controllers are required")
	}
	return nil
}
