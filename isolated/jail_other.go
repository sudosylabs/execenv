//go:build !linux

package isolated

import "github.com/sudosylabs/execenv"

func bindReadOnly(string, string) error { return execenv.ErrUnavailable }

func setTapOwner(string, int) error { return execenv.ErrUnavailable }

func cgroupV2() bool { return false }

func killJailed(*jailUIDs) {}

func removeJailCgroups(string) {}
