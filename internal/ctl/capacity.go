package ctl

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/sudosylabs/execenv/daemon"
)

// What bootstrap leaves for the host itself when it measures the machine.
const (
	hostMemoryReserve = 1 << 30
	hostDiskShare     = 10 // percent of free space kept back
	// Used when a measurement is not available on this platform.
	fallbackMemory = 4 << 30
	fallbackDisk   = 64 << 30
)

// measureCapacity is what this machine can give grants: every CPU, its
// memory less a reserve for the host, and the free space under state less a
// share kept back.
func measureCapacity(state string) daemon.Resources {
	out := daemon.Resources{CPUMillis: int64(runtime.NumCPU()) * 1000}
	if total, ok := memoryTotal(); ok {
		out.MemoryBytes = max(total-max(hostMemoryReserve, total/10), total/2)
	} else {
		out.MemoryBytes = fallbackMemory
	}
	if free, ok := diskFree(state); ok {
		out.DiskBytes = free - free*hostDiskShare/100
	} else {
		out.DiskBytes = fallbackDisk
	}
	return out
}

func memoryTotal() (int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil || kib <= 0 {
				return 0, false
			}
			return kib << 10, true
		}
	}
	return 0, false
}

// capacityFor is the capacity bootstrap writes: the operator's values where
// given, else what an earlier bootstrap wrote, else the machine measured.
func capacityFor(opts Options, existing daemon.Resources) daemon.Resources {
	out := existing
	if out == (daemon.Resources{}) {
		out = measureCapacity(opts.State)
	}
	if opts.Capacity.CPUMillis > 0 {
		out.CPUMillis = opts.Capacity.CPUMillis
	}
	if opts.Capacity.MemoryBytes > 0 {
		out.MemoryBytes = opts.Capacity.MemoryBytes
	}
	if opts.Capacity.DiskBytes > 0 {
		out.DiskBytes = opts.Capacity.DiskBytes
	}
	return out
}
