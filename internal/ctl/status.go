package ctl

import (
	"fmt"
	"io"
	"os/exec"
)

// Status reports isolation device usability, unit state, and installed
// ids. It never prints the token or full hashes.
func Status(opts Options, stdout io.Writer) error {
	opts = opts.resolved()
	devicePath := opts.Device
	if doc, err := loadExisting(opts.configPath()); err == nil && doc.Device != "" {
		devicePath = doc.Device
	}
	device := "missing"
	if checkDevice(devicePath) == nil {
		device = "ok"
	}
	unit := unitState(opts)
	if stdout != nil {
		fmt.Fprintf(stdout, "device=%s\n", device)
		fmt.Fprintf(stdout, "unit=%s\n", unit)
		fmt.Fprintf(stdout, "installed=%s\n", joinIDs(installedIDs(opts)))
	}
	return nil
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
