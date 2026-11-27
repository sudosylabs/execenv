//go:build !linux

package hostplatform

import "errors"

const Device = "/dev/kvm"

func CheckDevice(string) error { return errors.New("KVM requires Linux") }

func CheckCgroups() error { return errors.New("cgroup v2 requires Linux") }
