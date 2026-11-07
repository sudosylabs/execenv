//go:build isolation

package isolated

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

// These tests talk to real isolation: the virtualization device and
// microVM binaries for vm, the sandbox runtime for user-kernel. They are
// not part of make check. Run as root on the target host:
//
//	EXECENV_ISOLATION=1 EXECENV_ISOLATION_CLASSES=vm,user-kernel \
//	EXECENV_FIXTURE_KERNEL=... EXECENV_FIXTURE_ROOTFS=... \
//	go test -tags=isolation ./isolated
//
// EXECENV_ISOLATION_CLASSES defaults to vm. The fixture must be rebaked
// with the current guest init (per-grant disk, scratch home).

func liveClasses(t *testing.T) []execenv.Isolation {
	t.Helper()
	if os.Getenv("EXECENV_ISOLATION") != "1" {
		t.Skip("set EXECENV_ISOLATION=1 on a machine with isolation hardware")
	}
	if runtime.GOOS != "linux" {
		t.Skip("isolation tests are linux-only")
	}
	raw := os.Getenv("EXECENV_ISOLATION_CLASSES")
	if raw == "" {
		raw = string(execenv.IsolationVM)
	}
	var out []execenv.Isolation
	for _, class := range strings.Split(raw, ",") {
		out = append(out, execenv.Isolation(strings.TrimSpace(class)))
	}
	return out
}

func liveHost(t *testing.T, class execenv.Isolation, slots int, workDir string) *Host {
	t.Helper()
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
		Isolation: class,
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

type liveOnly struct{ execenv.Host }

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

// TestLiveConformance runs the shared contract suite against each real
// boundary, including one PTY, projection, harvest, freeze/thaw and revoke.
func TestLiveConformance(t *testing.T) {
	for _, class := range liveClasses(t) {
		t.Run(string(class), func(t *testing.T) {
			execenvtest.Run(t, func(t *testing.T) execenv.Host {
				t.Helper()
				host := liveHost(t, class, 2, "")
				t.Cleanup(func() { revokeAll(host) })
				// Hide the host-side GuestWriter hook: with a real boundary it
				// writes beside the guest, not into it. Guest writes are proven
				// through the shell in the tests below.
				return liveOnly{host}
			})
		})
	}
}

func TestLiveTouchAppearsOnWatch(t *testing.T) {
	for _, class := range liveClasses(t) {
		t.Run(string(class), func(t *testing.T) {
			host := liveHost(t, class, 1, "")
			t.Cleanup(func() { revokeAll(host) })
			env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
			if err != nil {
				t.Fatal(err)
			}
			obs, err := env.Watch(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = obs.Close() })
			term := liveShell(t, env)
			if _, err := term.Write([]byte("touch seen.txt\n")); err != nil {
				t.Fatal(err)
			}
			waitLiveEvent(t, obs, "seen.txt")
		})
	}
}

// TestLiveScratchHomeAndDiskWorkspace proves the shell's HOME is outside
// the watched tree and that the workspace is disk, not guest memory: it
// writes more than the grant's memory into /workspace.
func TestLiveScratchHomeAndDiskWorkspace(t *testing.T) {
	for _, class := range liveClasses(t) {
		t.Run(string(class), func(t *testing.T) {
			host := liveHost(t, class, 1, "")
			t.Cleanup(func() { revokeAll(host) })
			env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
			if err != nil {
				t.Fatal(err)
			}
			obs, err := env.Watch(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = obs.Close() })
			term := liveShell(t, env)
			script := "mkdir -p \"$HOME/.cache/pip\" && echo x > \"$HOME/.cache/pip/y\" && " +
				"dd if=/dev/zero of=/workspace/big bs=1M count=256 2>/dev/null && rm /workspace/big && " +
				"printf '%s\\n%s\\n' \"$HOME\" \"$(pwd)\" > where.txt\n"
			if _, err := term.Write([]byte(script)); err != nil {
				t.Fatal(err)
			}
			waitLiveEvent(t, obs, "where.txt")
			body := openEventually(t, env, "where.txt")
			want := execenv.GuestHome + "\n" + execenv.GuestWorkspace + "\n"
			if string(body) != want {
				t.Fatalf("where.txt = %q, want %q", body, want)
			}
		})
	}
}

// TestLiveStaleCleanupAfterCrash abandons a running grant without revoke,
// as a crashed daemon would, then constructs a new host on the same work
// directory. The new host must release the sandbox and mounts.
func TestLiveStaleCleanupAfterCrash(t *testing.T) {
	for _, class := range liveClasses(t) {
		if class != execenv.IsolationUserKernel {
			continue
		}
		t.Run(string(class), func(t *testing.T) {
			workDir := t.TempDir()
			first := liveHost(t, class, 1, workDir)
			if _, err := first.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
				t.Fatal(err)
			}
			_ = first.lock.Close()
			second := liveHost(t, class, 1, workDir)
			t.Cleanup(func() { revokeAll(second) })
			bin, err := lookBinary("", defaultSandboxRuntime)
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(bin, "--root", filepath.Join(workDir, sandboxState), "list", "-quiet").Output()
			if err == nil && len(bytes.TrimSpace(out)) != 0 {
				t.Fatalf("stale sandboxes survived: %s", out)
			}
			mounts, err := os.ReadFile("/proc/self/mountinfo")
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(mounts, []byte(workDir+"/")) {
				t.Fatal("stale mounts survived under the work directory")
			}
		})
	}
}

func TestLiveReadyRequiresDevice(t *testing.T) {
	for _, class := range liveClasses(t) {
		t.Run(string(class), func(t *testing.T) {
			host, err := New(Config{
				Isolation: class,
				WorkDir:   t.TempDir(),
				Slots:     1,
				Images:    []Image{writeCatalogImage(t, t.TempDir(), "default", "rootfs")},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = host.lock.Close() })
			report, err := host.Ready(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !report.Usable || report.Isolation != class {
				t.Fatalf("Ready() = %+v on a %s host", report, class)
			}
		})
	}
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

func waitLiveEvent(t *testing.T, obs execenv.Observation, path string) {
	t.Helper()
	deadline, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		ev, err := obs.Next(deadline)
		if err != nil {
			t.Fatalf("Watch did not see %s: %v", path, err)
		}
		if ev.Path == path && (ev.Op == execenv.OpCreate || ev.Op == execenv.OpReplace) {
			return
		}
	}
}

func openEventually(t *testing.T, env execenv.Env, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		body, err := env.Open(t.Context(), path)
		if err == nil {
			data, readErr := io.ReadAll(body)
			_ = body.Close()
			if readErr == nil && len(data) > 0 {
				return data
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Open(%s) never returned content: %v", path, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
