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
		WorkDir:   workDir,
		Slots:     slots,
		DiskBytes: 512 << 20,
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
	execenvtest.Run(t, func(t *testing.T) execenv.Host {
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
	term := liveShell(t, env)
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
	term := liveShell(t, env)
	script := "mkdir -p \"$HOME/.cache/pip\" && echo x > \"$HOME/.cache/pip/y\" && " +
		"dd if=/dev/zero of=/workspace/big bs=1M count=256 2>/dev/null && rm /workspace/big && " +
		"printf '%s\\n%s\\n' \"$HOME\" \"$(pwd)\" > where.txt\n"
	if _, err := term.Write([]byte(script)); err != nil {
		t.Fatal(err)
	}
	waitLiveContent(t, env, fence, "where.txt", execenv.GuestHome+"\n"+execenv.GuestWorkspace+"\n")
	page, err := env.Observe(t.Context(), fence, 0, 128)
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
// as a crashed daemon would, then constructs a new host on the same work
// directory. The new host must release the grant's processes, jails and
// grant links.
func TestLiveStaleCleanupAfterCrash(t *testing.T) {
	workDir := t.TempDir()
	first := liveHost(t, 1, workDir)
	if _, err := first.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
		t.Fatal(err)
	}
	// A crash: the jailed monitor keeps running, the lock is released.
	_ = first.lock.Close()
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
	if _, err := os.Stat(filepath.Join(workDir, jailsDir)); err == nil {
		if left, _ := os.ReadDir(filepath.Join(workDir, jailsDir)); len(left) != 0 {
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
		WorkDir: t.TempDir(),
		Slots:   1,
		Images:  []Image{writeCatalogImage(t, t.TempDir(), "default", "rootfs")},
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

func liveShell(t *testing.T, env execenv.Env) execenv.Terminal {
	t.Helper()
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	go func() { _, _ = io.Copy(io.Discard, term) }()
	return term
}

// waitLiveObservation polls Observe until a create or write of path appears
// and returns the latest such observation.
func waitLiveObservation(t *testing.T, env execenv.Env, fence execenv.ProjectionFence, path string) execenv.ExecutionObservation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		page, err := env.Observe(t.Context(), fence, 0, 128)
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
