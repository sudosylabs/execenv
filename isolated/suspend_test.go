package isolated

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// An idle grant set to suspend is saved and releases its CPU and memory;
// the caller's thaw restores it with its processes and their state.
func TestIdleSuspendReleasesAndResumes(t *testing.T) {
	launch := &recordingLauncher{}
	h := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "dozing", Image: "default", IdleTimeout: 150 * time.Millisecond, IdleAction: execenv.IdleSuspend}
	env, err := h.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	sh := execenvtest.StartShell(t, env, fence, "sh")
	if err := sh.Type(`kept=asleep; echo set""-done`); err != nil {
		t.Fatal(err)
	}
	sh.Expect("set-done")
	_ = sh.Attachment().Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		report, err := h.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		receipt, _ := env.ControlStatus(t.Context())
		if receipt.State == execenv.ControlFrozen && report.MemoryBytes.Reserved == 0 && report.DiskBytes.Reserved > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not suspended: %+v %+v", receipt, report.MemoryBytes)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(statePath(h.cfg.WorkDir, "dozing")); err != nil {
		t.Fatal(err)
	}
	// An idle suspend keeps the caller's revision; it thaws with its next.
	thawed := fence
	thawed.FenceRevision++
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: thawed, State: execenv.ControlRunning}); err != nil {
		t.Fatalf("thaw of a suspended grant: %v", err)
	}
	report, _ := h.Ready(t.Context())
	if report.MemoryBytes.Reserved == 0 || launch.restores != 1 || launch.starts != 1 {
		t.Fatalf("resume: reserved %+v, restores %d, starts %d", report.MemoryBytes, launch.restores, launch.starts)
	}
	info, err := env.Process(t.Context(), "sh")
	if err != nil || !info.Running {
		t.Fatalf("Process() after resume = %+v, %v", info, err)
	}
	typeAndExpect(t, env, thawed, "sh", info, "echo $kept-y", "asleep-y")
}

// A grant suspended when its host went away is adopted suspended, and
// resumes on thaw.
func TestRestartKeepsASuspendedGrant(t *testing.T) {
	launch := &recordingLauncher{}
	h := testHost(t, func() error { return nil }, launch)
	spec := execenv.Spec{ID: "paused", Image: "default", IdleTimeout: 150 * time.Millisecond, IdleAction: execenv.IdleSuspend}
	env, err := h.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	execenvtest.StartShell(t, env, fence, "sh").Attachment().Close()
	deadline := time.Now().Add(5 * time.Second)
	// A restart reads the state file, so wait for that to say suspended.
	for {
		states, _, _ := loadStates(h.cfg.WorkDir)
		if states["paused"].Suspended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not suspended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h2 := restart(t, h)
	report, _ := h2.Ready(t.Context())
	if report.MemoryBytes.Reserved != 0 || report.Grants != 1 {
		t.Fatalf("adopted suspended grant reserves %+v (grants %d)", report.MemoryBytes, report.Grants)
	}
	again, err := h2.Ensure(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	thawed := fence
	thawed.FenceRevision++
	if _, err := again.Control(t.Context(), execenv.ControlRequest{Fence: thawed, State: execenv.ControlRunning}); err != nil {
		t.Fatalf("thaw after restart: %v", err)
	}
	if info, err := again.Process(context.Background(), "sh"); err != nil || !info.Running {
		t.Fatalf("Process() = %+v, %v", info, err)
	}
}

// A grant starts from the template of its image with exactly its memory
// and the fewest virtual CPUs that cover it; a grant with a network, or
// another memory size, boots.
func TestTemplatePick(t *testing.T) {
	small := Shape{Name: "small", VCPUs: 1, MemoryBytes: 512 << 20}
	wide := Shape{Name: "wide", VCPUs: 4, MemoryBytes: 512 << 20}
	big := Shape{Name: "big", VCPUs: 2, MemoryBytes: 1 << 30}
	tpl := &templates{records: map[templateKey]*templateRecord{
		{"default", "small"}: {image: "default", shape: small, ready: true},
		{"default", "wide"}:  {image: "default", shape: wide, ready: true},
		{"default", "big"}:   {image: "default", shape: big, ready: false},
	}}
	spec := func(cpu, mem int64, network execenv.Network) execenv.Spec {
		return execenv.Spec{ID: "g", Image: "default", Network: network, Resources: execenv.Resources{CPUMillis: cpu, MemoryBytes: mem}}
	}
	if r, ok := tpl.pick(spec(500, 512<<20, execenv.NetworkNone)); !ok || r.shape.Name != "small" {
		t.Fatalf("half a CPU picked %+v %v", r.shape, ok)
	}
	if r, ok := tpl.pick(spec(2500, 512<<20, execenv.NetworkNone)); !ok || r.shape.Name != "wide" {
		t.Fatalf("2.5 CPUs picked %+v %v", r.shape, ok)
	}
	for _, s := range []execenv.Spec{
		spec(500, 768<<20, execenv.NetworkNone),
		spec(500, 512<<20, execenv.NetworkAllowlist),
		spec(5000, 512<<20, execenv.NetworkNone),
		spec(1000, 1<<30, execenv.NetworkNone),
	} {
		if r, ok := tpl.pick(s); ok {
			t.Fatalf("%+v picked %+v", s.Resources, r.shape)
		}
	}
}
