//go:build !linux

package isolated

// checkDeviceNodes has nothing to check where grants cannot run.
func checkDeviceNodes(string) error { return nil }
