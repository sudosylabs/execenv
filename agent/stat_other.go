//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package agent

import "os"

// statOf never trusts metadata where the platform exposes none, so every
// rescan hashes. Guests are Linux.
func statOf(os.FileInfo, int64) statKey { return statKey{} }
