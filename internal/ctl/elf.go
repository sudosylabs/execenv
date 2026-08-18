package ctl

import (
	"encoding/binary"
	"fmt"
	"os"
)

const (
	elfClass64    = 2
	elfData2LSB   = 1
	elfOSABISysV  = 0
	elfOSABILinux = 3
	elfMachineX64 = 62
	elfMachineARM = 183
)

func requireLinuxAMD64(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return wrap("upgrade", err)
	}
	if len(raw) < 20 || raw[0] != 0x7f || raw[1] != 'E' || raw[2] != 'L' || raw[3] != 'F' {
		return wrap("upgrade", fmt.Errorf("refusing a non-linux binary"))
	}
	if raw[4] != elfClass64 || raw[5] != elfData2LSB {
		return wrap("upgrade", fmt.Errorf("refusing a non-linux amd64 binary"))
	}
	if raw[7] != elfOSABISysV && raw[7] != elfOSABILinux {
		return wrap("upgrade", fmt.Errorf("refusing a non-linux amd64 binary"))
	}
	switch binary.LittleEndian.Uint16(raw[18:20]) {
	case elfMachineX64:
		return nil
	default:
		return wrap("upgrade", fmt.Errorf("refusing a non-linux amd64 binary"))
	}
}
