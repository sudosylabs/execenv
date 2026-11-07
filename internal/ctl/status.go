package ctl

import (
	"fmt"
	"io"
	"os/exec"

	"github.com/sudosylabs/execenv"
)

// Status reports the configured isolation class, isolation device or
// sandbox runtime usability, unit state, and installed ids. It never
// prints the token or full hashes.
func Status(opts Options, stdout io.Writer) error {
	opts = opts.resolved()
	devicePath := opts.Device
	isolation := isolationVM
	runtimePath := ""
	doc, err := loadHost(opts.configPath())
	if err == nil {
		if doc.Device != "" {
			devicePath = doc.Device
		}
		if doc.Isolation != "" {
			isolation = doc.Isolation
		}
		runtimePath = doc.Runtime
	}
	device := "missing"
	if isolation == isolationUserKernel {
		device = "not-required"
	} else if checkDevice(devicePath) == nil {
		device = "ok"
	}
	unit := unitState(opts)
	if stdout != nil {
		fmt.Fprintf(stdout, "isolation=%s\n", isolation)
		fmt.Fprintf(stdout, "device=%s\n", device)
		if isolation == isolationUserKernel {
			state := "missing"
			if runtimePath != "" && executable(runtimePath) {
				state = "ok"
			}
			fmt.Fprintf(stdout, "sandbox-runtime=%s\n", state)
		}
		mkfs := "missing"
		if doc.Mkfs != "" && executable(doc.Mkfs) {
			mkfs = "ok"
		} else if _, err := exec.LookPath(mkfsName); err == nil {
			mkfs = "ok"
		}
		fmt.Fprintf(stdout, "mkfs=%s\n", mkfs)
		fmt.Fprintf(stdout, "unit=%s\n", unit)
		fmt.Fprintf(stdout, "installed=%s\n", joinIDs(installedIDs(opts)))
		fmt.Fprintf(stdout, "release=%s\n", stampRelease())
	}
	return nil
}

func stampRelease() string {
	if execenv.Tag != "" {
		return execenv.Tag
	}
	return execenv.Release
}

func unitState(opts Options) string {
	if _, err := exec.LookPath("systemctl"); err == nil {
		if err := exec.Command("systemctl", "is-active", "--quiet", unitName).Run(); err == nil {
			return "active"
		}
	}
	if fileExists(opts.unitPath()) {
		return "inactive"
	}
	return "unknown"
}
