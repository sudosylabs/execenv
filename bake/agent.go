package bake

import "debug/elf"

// LinuxAgent reports whether path is an ELF guest binary. Bake installs
// this file as PID 1's helper; a macOS or Windows execenv will not start
// inside the guest.
func LinuxAgent(path string) bool {
	file, err := elf.Open(path)
	if err != nil {
		return false
	}
	machine := file.Machine
	_ = file.Close()
	switch machine {
	case elf.EM_X86_64, elf.EM_AARCH64, elf.EM_386, elf.EM_ARM:
		return true
	default:
		return false
	}
}
