//go:build !linux

package isolated

import "github.com/sudosylabs/execenv"

func bindReadOnly(string, string) error { return execenv.ErrUnavailable }

func setTapOwner(string, int) error { return execenv.ErrUnavailable }

func killJailed(*jailUIDs, map[int]bool) {}

func removeJailCgroups(string) {}
