//go:build isolation && linux

package isolated

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// These tests talk to real isolation: the virtualization device and the
// microVM binaries. They are not part of make check. Run as root on the
// target host:
//
//	EXECENV_ISOLATION=1 EXECENV_FIXTURE_KERNEL=... EXECENV_FIXTURE_ROOTFS=... \
//	go test -tags=isolation ./isolated
//
// The fixture must be rebaked with the current guest init (per-grant disk,
// scratch home, contained workload).

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("EXECENV_ISOLATION") != "1" {
		t.Skip("set EXECENV_ISOLATION=1 on a machine with isolation hardware")
	}
	if runtime.GOOS != "linux" {
		t.Skip("isolation tests are linux-only")
	}
}

// liveGrant is the size of every live test grant: small enough that a
// laptop runs a handful at once.
var liveGrant = execenv.Resources{CPUMillis: 1000, MemoryBytes: 256 << 20, DiskBytes: 512 << 20}

func liveHost(t *testing.T, slots int, workDir string) *Host {
	t.Helper()
	requireLive(t)
	kernel := os.Getenv("EXECENV_FIXTURE_KERNEL")
	rootfs := os.Getenv("EXECENV_FIXTURE_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Fatal("set EXECENV_FIXTURE_KERNEL and EXECENV_FIXTURE_ROOTFS to a tiny image that starts the guest agent")
	}
	hash, err := Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	if workDir == "" {
		workDir = t.TempDir()
	}
	host, err := New(Config{
		WorkDir:  workDir,
		Capacity: liveGrant.Times(int64(slots)),
		Defaults: liveGrant,
		Images: []Image{
			{ID: "default", Kernel: kernel, Rootfs: rootfs, Hash: hash},
			{ID: "alt", Kernel: kernel, Rootfs: rootfs, Hash: hash},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func revokeAll(h *Host) {
	h.mu.Lock()
	ids := make([]execenv.ID, 0, len(h.grants))
	for id := range h.grants {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		_ = h.Revoke(context.Background(), id)
	}
	_ = h.lock.Close()
}

// TestLiveConformance runs the shared contract suite against the real
// boundary, including one PTY, journal projection, fenced control and revoke.
func TestLiveConformance(t *testing.T) {
	requireLive(t)
	execenvtest.Run(t, func(t *testing.T, cfg execenvtest.Config) execenv.Host {
		t.Helper()
		host := liveHost(t, 2, "")
		t.Cleanup(func() { revokeAll(host) })
		return host
	})
}

func TestLiveTouchIsObserved(t *testing.T) {
	host := liveHost(t, 1, "")
	t.Cleanup(func() { revokeAll(host) })
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := liveJournal(t, env)
	term := liveShell(t, env, fence)
	if _, err := term.Write([]byte("touch seen.txt\n")); err != nil {
		t.Fatal(err)
	}
	waitLiveObservation(t, env, fence, "seen.txt")
}

// TestLiveScratchHomeAndDiskWorkspace proves the shell's HOME is outside
// the projected tree and that the workspace is disk, not guest memory: it
// writes more than the grant's memory into /workspace.
func TestLiveScratchHomeAndDiskWorkspace(t *testing.T) {
	host := liveHost(t, 1, "")
	t.Cleanup(func() { revokeAll(host) })
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := liveJournal(t, env)
	term := liveShell(t, env, fence)
	script := "mkdir -p \"$HOME/.cache/pip\" && echo x > \"$HOME/.cache/pip/y\" && " +
		"dd if=/dev/zero of=/workspace/big bs=1M count=256 2>/dev/null && rm /workspace/big && " +
		"printf '%s\\n%s\\n' \"$HOME\" \"$(pwd)\" > where.txt\n"
	if _, err := term.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	waitLiveContent(t, env, fence, "where.txt", execenv.GuestHome+"\n"+execenv.GuestWorkspace+"\n")
	page, err := env.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: 0, Limit: 128})
	if err != nil {
		t.Fatal(err)
	}
	for _, observed := range page.Observations {
		if strings.HasPrefix(observed.Path, ".cache") {
			t.Fatalf("scratch HOME observed: %+v", observed)
		}
	}
}

// TestLiveStaleCleanupAfterCrash abandons a running grant without revoke,
// as a crashed daemon would, and loses its state file, so the new host on
// the same work directory has nothing to adopt it by. The new host must
// release the grant's processes, jails and grant links.
func TestLiveStaleCleanupAfterCrash(t *testing.T) {
	workDir := t.TempDir()
	first := liveHost(t, 1, workDir)
	if _, err := first.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
		t.Fatal(err)
	}
	// A crash: the jailed monitor keeps running, the lock is released.
	_ = first.lock.Close()
	if err := os.Remove(statePath(workDir, "grant-1")); err != nil {
		t.Fatal(err)
	}
	second := liveHost(t, 1, workDir)
	t.Cleanup(func() { revokeAll(second) })
	pool := newJailUIDs(0)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if uid, ok := procUID(filepath.Join("/proc", entry.Name(), "status")); ok && pool.contains(uid) {
			t.Fatalf("jailed process %s survived restart", entry.Name())
		}
	}
	execs, _ := os.ReadDir(filepath.Join(workDir, jailsDir))
	for _, exec := range execs {
		if left, _ := os.ReadDir(filepath.Join(workDir, jailsDir, exec.Name())); len(left) != 0 {
			t.Fatalf("stale jails survived: %v", left)
		}
	}
	links, _ := os.ReadDir("/sys/class/net")
	for _, link := range links {
		if _, err := os.Stat("/sys/class/net/" + link.Name() + "/tun_flags"); err == nil && staleGrantDev(link.Name()) {
			t.Fatalf("stale grant tap %s survived restart", link.Name())
		}
	}
}

func TestLiveReadyRequiresDevice(t *testing.T) {
	requireLive(t)
	host, err := New(Config{
		WorkDir:  t.TempDir(),
		Capacity: execenv.DefaultGrant.Times(1),
		Images:   []Image{writeCatalogImage(t, t.TempDir(), "default", "rootfs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.lock.Close() })
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Usable || report.Isolation != execenv.IsolationVM {
		t.Fatalf("Ready() = %+v on a vm host", report)
	}
}

// liveJournal enters the fenced protocol and initializes an empty
// projection, so guest writes become observations.
func liveJournal(t *testing.T, env execenv.Env) execenv.ProjectionFence {
	t.Helper()
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init"}); err != nil {
		t.Fatal(err)
	}
	return fence
}

func liveShell(t *testing.T, env execenv.Env, fence execenv.ProjectionFence) *execenvtest.ShellConn {
	t.Helper()
	term := execenvtest.OpenShell(t, env, fence, "sh")
	go func() { _, _ = io.Copy(io.Discard, term) }()
	return term
}

// waitLiveObservation polls Observe until a create or write of path appears
// and returns the latest such observation.
func waitLiveObservation(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, path string) execenv.ExecutionObservation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		page, err := env.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: 0, Limit: 128})
		if err != nil {
			t.Fatal(err)
		}
		for i := len(page.Observations) - 1; i >= 0; i-- {
			event := page.Observations[i]
			if event.Path == path && (event.Operation == execenv.ObservationCreateFile || event.Operation == execenv.ObservationWriteFile) {
				return event
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no observation of %s", path)
	return execenv.ExecutionObservation{}
}

// waitLiveContent polls until the latest observation of path captured want.
// A shell redirect may be observed before its body is complete.
func waitLiveContent(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, path, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		event := waitLiveObservation(t, env, fence, path)
		reader, err := env.OpenObservationContent(t.Context(), fence, *event.Content)
		if err != nil {
			t.Fatal(err)
		}
		got, err = io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s = %q, want %q", path, got, want)
}

// TestLiveProcessConformance runs the process log suite through the real
// guest kernel's process events: the shell types real commands, and only
// the contained workload's programs are recorded.
func TestLiveProcessConformance(t *testing.T) {
	requireLive(t)
	execenvtest.RunProcesses(t, func(t *testing.T) execenvtest.Traced {
		t.Helper()
		host := liveHost(t, 1, "")
		t.Cleanup(func() { revokeAll(host) })
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "traced", Image: "default"})
		if err != nil {
			t.Fatal(err)
		}
		fence := runningFence(t, env)
		return execenvtest.Traced{Env: env, Fence: fence, Workload: execenvtest.ShellWorkload(t, env, fence)}
	})
}

// TestLiveObservationConformance runs the observation suite through a real
// guest: waits end on real changes, and each change names the started
// process whose descendant made it, through the guest kernel's process
// events and its filesystem events.
func TestLiveObservationConformance(t *testing.T) {
	requireLive(t)
	execenvtest.RunObservations(t, func(t *testing.T) execenvtest.Observed {
		t.Helper()
		host := liveHost(t, 1, "")
		t.Cleanup(func() { revokeAll(host) })
		env, err := host.Ensure(t.Context(), execenv.Spec{ID: "observed", Image: "default"})
		if err != nil {
			t.Fatal(err)
		}
		fence := runningFence(t, env)
		return execenvtest.Observed{Workspace: env, Fence: fence, Guest: execenvtest.FilesGuest(env, fence), Processes: env}
	})
}

// An Observe after one change in a 5,000-file workspace reads that change,
// not the workspace: it reports exactly it, and it is quick.
func TestLiveObserveCostsTheChangeNotTheTree(t *testing.T) {
	host := liveHost(t, 1, "")
	t.Cleanup(func() { revokeAll(host) })
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "big-tree", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := liveJournal(t, env)
	sh := execenvtest.StartShell(t, env, fence, "sh")
	// Built in halves and acknowledged as read: the log keeps at most 4096
	// unacknowledged events.
	var cursor uint64
	for _, half := range []string{"1 25", "26 50"} {
		if err := sh.Type(`for d in $(seq ` + half + `); do mkdir -p t/$d; for f in $(seq 1 100); do echo $d-$f > t/$d/$f; done; done; echo half""-made`); err != nil {
			t.Fatal(err)
		}
		sh.Expect("half-made")
		sh = execenvtest.StartShell(t, env, fence, "sh-"+strings.ReplaceAll(half, " ", "-"))
		for {
			page, err := env.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: cursor, Limit: 128})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Observations) == 0 {
				break
			}
			cursor = page.Observations[len(page.Observations)-1].HostSequence
			if err := env.AcknowledgeObservations(t.Context(), fence, cursor); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := sh.Type(`echo changed > t/25/50; echo one""-change`); err != nil {
		t.Fatal(err)
	}
	sh.Expect("one-change")
	started := time.Now()
	page, err := env.Observe(t.Context(), execenv.ObserveRequest{Fence: fence, After: cursor, Limit: 128})
	took := time.Since(started)
	if err != nil || len(page.Observations) != 1 || page.Observations[0].Path != "t/25/50" {
		t.Fatalf("Observe() after one change = %+v, %v", page.Observations, err)
	}
	t.Logf("Observe after one change in 5,000 files took %v", took)
}

func TestLivePortConformance(t *testing.T) {
	host := liveHost(t, 1, "")
	t.Cleanup(func() { revokeAll(host) })
	execenvtest.RunPorts(t, host, execenvtest.GuestPortListener(t))
}

func TestLiveListeningPortsConformance(t *testing.T) {
	host := liveHost(t, 1, "")
	t.Cleanup(func() { revokeAll(host) })
	execenvtest.RunListeningPorts(t, host, execenvtest.GuestPortListener(t))
}
