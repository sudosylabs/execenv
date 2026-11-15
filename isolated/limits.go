package isolated

import (
	"context"
	"strconv"

	"github.com/sudosylabs/execenv"
)

// cgroupPeriod is the CPU accounting period, in microseconds.
const cgroupPeriod = 100_000

// monitorOverhead is memory the monitor process itself needs beyond guest
// memory. The cgroup's memory limit allows for it.
const monitorOverhead = 64 << 20

// vcpuCount is enough virtual CPUs to use cpuMillis of CPU time.
func vcpuCount(cpuMillis int64) int64 {
	return max(1, (cpuMillis+999)/1000)
}

// cgroupLimits are the supervisor's cgroup arguments for one grant: its
// CPU time per period and its memory, so a grant cannot take more than it
// reserved however many virtual CPUs it sees.
func cgroupLimits(r execenv.Resources) []string {
	quota := r.CPUMillis * cgroupPeriod / 1000
	return []string{
		"--cgroup", "cpu.max=" + strconv.FormatInt(quota, 10) + " " + strconv.Itoa(cgroupPeriod),
		"--cgroup", "memory.max=" + strconv.FormatInt(r.MemoryBytes+monitorOverhead, 10),
		"--cgroup", "memory.swap.max=0",
	}
}

// rateLimiter is the monitor's rate limiter for a byte bucket and an
// operation bucket, refilled every second. A zero rate is no limit.
func rateLimiter(bytesPerSecond, opsPerSecond execenv.TokenBucket) map[string]any {
	limiter := map[string]any{}
	if b := bucket(bytesPerSecond); b != nil {
		limiter["bandwidth"] = b
	}
	if b := bucket(opsPerSecond); b != nil {
		limiter["ops"] = b
	}
	return limiter
}

func bucket(b execenv.TokenBucket) map[string]any {
	if b.Rate == 0 {
		return nil
	}
	out := map[string]any{"size": b.Rate, "refill_time": 1000}
	if b.Burst > 0 {
		out["one_time_burst"] = b.Burst
	}
	return out
}

// UpdateLimits patches the running machine's devices through its API socket.
func (p *processInstance) UpdateLimits(ctx context.Context, limits execenv.Limits, network bool) error {
	m := newMonitor(p.api)
	defer m.close()
	if err := m.patch(ctx, "/drives/workspace", map[string]any{
		"drive_id":     "workspace",
		"rate_limiter": rateLimiter(limits.DiskBytes, limits.DiskOps),
	}); err != nil {
		return err
	}
	if !network {
		return nil
	}
	return m.patch(ctx, "/network-interfaces/eth0", map[string]any{
		"iface_id":        "eth0",
		"rx_rate_limiter": rateLimiter(limits.NetworkBytes, execenv.TokenBucket{}),
		"tx_rate_limiter": rateLimiter(limits.NetworkBytes, execenv.TokenBucket{}),
	})
}
