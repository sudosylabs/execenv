//go:build isolation && linux

package isolated

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// liveShapedHost is a live host that builds a template of the live grant's
// size and waits until it is ready.
func liveShapedHost(t *testing.T, slots int) *Host {
	t.Helper()
	requireLive(t)
	kernel := os.Getenv("EXECENV_FIXTURE_KERNEL")
	rootfs := os.Getenv("EXECENV_FIXTURE_ROOTFS")
	hash, err := Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	host, err := New(Config{
		WorkDir:  t.TempDir(),
		Capacity: liveGrant.Times(int64(slots + 1)),
		Defaults: liveGrant,
		Images:   []Image{{ID: "default", Kernel: kernel, Rootfs: rootfs, Hash: hash}},
		Shapes:   []Shape{{Name: "live", VCPUs: int(vcpuCount(liveGrant.CPUMillis)), MemoryBytes: liveGrant.MemoryBytes}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.templates.stop(); revokeAll(host) })
	deadline := time.Now().Add(5 * time.Minute)
	for {
		report, err := host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Templates) == 1 && report.Templates[0].Ready {
			return host
		}
		if time.Now().After(deadline) {
			t.Fatalf("template not ready: %+v", report.Templates)
		}
		time.Sleep(time.Second)
	}
}

// liveOutput types command into a fresh shell and returns what it printed.
func liveOutput(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, command string) string {
	t.Helper()
	sh := execenvtest.StartShell(t, env, fence, "sh")
	if err := sh.Type(command + "; echo end\"\"-of-output"); err != nil {
		t.Fatal(err)
	}
	sh.Expect("end-of-output")
	return sh.Seen()
}

// Two grants restored from one template are each their own: their random
// generators, machine ids and workspaces differ, and each has its epoch.
func TestLiveTemplateGrantsAreIndependent(t *testing.T) {
	host := liveShapedHost(t, 2)
	probe := "cat /proc/sys/kernel/random/uuid; cat /etc/machine-id; echo $RANDOM; ls /workspace"
	var seen []string
	for _, id := range []execenv.ID{"tpl-a", "tpl-b"} {
		started := time.Now()
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: id, Image: "default"})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s started in %v", id, time.Since(started))
		fence := runningFence(t, env)
		if _, err := env.WriteFile(t.Context(), fence, string(id)+".txt", strings.NewReader("mine"), execenv.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, liveOutput(t, env, fence, probe))
	}
	if seen[0] == seen[1] {
		t.Fatal("two grants from one template are the same machine")
	}
	if strings.Contains(seen[0], "tpl-b.txt") || strings.Contains(seen[1], "tpl-a.txt") {
		t.Fatal("grants from one template share a workspace")
	}
}

// A suspended grant releases its memory and resumes with its shell's state.
func TestLiveSuspendResumes(t *testing.T) {
	host := liveHost(t, 1, "")
	t.Cleanup(func() { revokeAll(host) })
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "sleeper", Image: "default", IdleTimeout: 2 * time.Second, IdleAction: execenv.IdleSuspend})
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	sh := execenvtest.StartShell(t, env, fence, "sh")
	if err := sh.Type("kept=asleep; echo set\"\"-done"); err != nil {
		t.Fatal(err)
	}
	sh.Expect("set-done")
	_ = sh.Attachment().Close()
	deadline := time.Now().Add(time.Minute)
	for {
		report, err := host.Ready(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if report.MemoryBytes.Reserved == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not suspended: %+v", report.MemoryBytes)
		}
		time.Sleep(200 * time.Millisecond)
	}
	thawed := fence
	thawed.FenceRevision++
	started := time.Now()
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: thawed, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	t.Logf("resumed in %v", time.Since(started))
	info, err := env.Process(t.Context(), "sh")
	if err != nil || !info.Running {
		t.Fatalf("Process() after resume = %+v, %v", info, err)
	}
	typeAndExpect(t, env, thawed, "sh", info, "echo $kept-y", "asleep-y")
}
