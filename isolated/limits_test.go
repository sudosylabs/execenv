package isolated

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sudosylabs/execenv"
)

func TestMachineIsSizedAndLimitedFromTheGrant(t *testing.T) {
	t.Parallel()
	grant := filepath.Join(t.TempDir(), "grant")
	req := startRequest{
		ID:        "grant-1",
		Kernel:    "/k",
		Rootfs:    "/r",
		TreeDir:   filepath.Join(grant, workspaceName),
		Disk:      filepath.Join(grant, diskName),
		Resources: execenv.Resources{CPUMillis: 1500, MemoryBytes: 768 << 20, DiskBytes: 4 << 30},
		Limits:    execenv.Limits{DiskBytes: execenv.TokenBucket{Rate: 8 << 20, Burst: 64 << 20}, DiskOps: execenv.TokenBucket{Rate: 500}},
	}
	jail := newJailLayout(filepath.Join(grant, "jails"), "/opt/bin/firecracker", req.ID, os.Getuid(), os.Getgid())
	if err := os.MkdirAll(jail.root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := writeMachineConfig(req, jail); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(jail.host(jailConfig))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Machine struct {
			VCPUs  int64 `json:"vcpu_count"`
			Memory int64 `json:"mem_size_mib"`
		} `json:"machine-config"`
		Drives []struct {
			ID      string `json:"drive_id"`
			Limiter struct {
				Bandwidth *struct {
					Size   int64 `json:"size"`
					Burst  int64 `json:"one_time_burst"`
					Refill int64 `json:"refill_time"`
				} `json:"bandwidth"`
				Ops *struct {
					Size int64 `json:"size"`
				} `json:"ops"`
			} `json:"rate_limiter"`
		} `json:"drives"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Machine.VCPUs != 2 || doc.Machine.Memory != 768 {
		t.Fatalf("machine = %+v, want 2 vCPUs for 1500 millis and 768 MiB", doc.Machine)
	}
	disk := doc.Drives[1]
	if disk.ID != "workspace" || disk.Limiter.Bandwidth == nil || disk.Limiter.Bandwidth.Size != 8<<20 || disk.Limiter.Bandwidth.Burst != 64<<20 || disk.Limiter.Bandwidth.Refill != 1000 {
		t.Fatalf("workspace limiter = %+v", disk.Limiter)
	}
	if disk.Limiter.Ops == nil || disk.Limiter.Ops.Size != 500 {
		t.Fatalf("workspace ops limiter = %+v", disk.Limiter.Ops)
	}
}

func TestCgroupThrottlesToTheReservedCPU(t *testing.T) {
	t.Parallel()
	args := cgroupLimits(execenv.Resources{CPUMillis: 1500, MemoryBytes: 1 << 30})
	for _, want := range []string{"cpu.max=150000 100000", "memory.high=1140850688", "memory.max=1409286144", "memory.swap.max=0"} {
		if !slices.Contains(args, want) {
			t.Errorf("cgroup args %v lack %q", args, want)
		}
	}
	if vcpuCount(100) != 1 || vcpuCount(1000) != 1 || vcpuCount(1001) != 2 {
		t.Fatal("vcpuCount rounds wrong")
	}
	if len(rateLimiter(execenv.TokenBucket{}, execenv.TokenBucket{})) != 0 {
		t.Fatal("a zero bucket must mean no limit")
	}
}

func TestPartialLimitsUpdateRestoresPreviousLimits(t *testing.T) {
	launch := &recordingLauncher{}
	h := testHost(t, func() error { return nil }, launch)
	h.cfg.Allow = []string{"192.0.2.1"}
	spec := execenv.Spec{ID: "limits", Image: "default", Network: execenv.NetworkAllowlist}
	env, err := h.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	inst := launch.instance(spec.ID)
	inst.mu.Lock()
	inst.limitErrors = []error{execenv.ErrUnavailable}
	inst.mu.Unlock()
	limits := execenv.Limits{DiskBytes: execenv.TokenBucket{Rate: 4096}, NetworkBytes: execenv.TokenBucket{Rate: 8192}}
	if err := env.UpdateLimits(t.Context(), fence, limits); !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("partially applied limits: %v", err)
	}
	inst.mu.Lock()
	current := inst.limits
	inst.mu.Unlock()
	if current != spec.Limits {
		t.Fatalf("limits after failure = %+v, want %+v", current, spec.Limits)
	}
	if err := env.UpdateLimits(t.Context(), fence, limits); err != nil {
		t.Fatal(err)
	}
	inst.mu.Lock()
	current = inst.limits
	inst.mu.Unlock()
	if current != limits {
		t.Fatalf("limits after retry = %+v, want %+v", current, limits)
	}
}
