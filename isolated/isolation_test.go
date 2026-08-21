//go:build isolation

package isolated

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

// These tests talk to a real isolation device and supervisor binaries.
// They are not part of make check. Run:
//
//	go test -tags=isolation ./isolated
func TestLiveTouchAppearsOnWatch(t *testing.T) {
	if os.Getenv("EXECENV_ISOLATION") != "1" {
		t.Skip("set EXECENV_ISOLATION=1 on a machine with isolation hardware")
	}
	if runtime.GOOS != "linux" {
		t.Skip("isolation tests are linux-only")
	}
	kernel := os.Getenv("EXECENV_FIXTURE_KERNEL")
	rootfs := os.Getenv("EXECENV_FIXTURE_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set EXECENV_FIXTURE_KERNEL and EXECENV_FIXTURE_ROOTFS to a tiny image that starts the guest agent")
	}
	hash, err := Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	host, err := New(Config{
		WorkDir: t.TempDir(),
		Slots:   1,
		Images: []Image{{
			ID:     "fixture",
			Kernel: kernel,
			Rootfs: rootfs,
			Hash:   hash,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Revoke(t.Context()) })
	obs, err := env.Watch(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte("touch seen.txt\n")); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for {
		ev, err := obs.Next(deadline)
		if err != nil {
			t.Fatalf("Watch did not see touch: %v", err)
		}
		if ev.Op == execenv.OpCreate && ev.Path == "seen.txt" {
			return
		}
	}
}

func TestLiveReadyRequiresDevice(t *testing.T) {
	if os.Getenv("EXECENV_ISOLATION") != "1" {
		t.Skip("set EXECENV_ISOLATION=1 on a machine with isolation hardware")
	}
	if runtime.GOOS != "linux" {
		t.Skip("isolation tests are linux-only")
	}
	host, err := New(Config{
		WorkDir: t.TempDir(),
		Slots:   1,
		Images:  []Image{writeCatalogImage(t, t.TempDir(), "default", "rootfs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Usable {
		t.Fatal("Ready() Usable = false on an isolation-enabled host")
	}
}
