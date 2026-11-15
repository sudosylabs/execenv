//go:build isolation

package daemon_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/daemon"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/internal/ctl"
	"github.com/sudosylabs/execenv/isolated"
	"github.com/sudosylabs/execenv/remote"
)

const harvestBody = "from-host\n"

// TestCertifyRemoteHarvestsGuestFile is the certification path: a
// bootstrapped host process, the client is remote.New, the process is a
// guest login shell, and a guest-created file is harvested as an observation with its
// captured content. Not an echo PTY.
func TestCertifyRemoteHarvestsGuestFile(t *testing.T) {
	client := startCertifyHost(t)

	env, err := client.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Revoke(t.Context(), "grant-1") })
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	content, err := env.UploadProjectionContent(t.Context(), fence, strings.NewReader(harvestBody))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{
		{EntryID: "hello", Kind: execenv.ProjectionFile, Path: "hello.txt", ContentVersion: "v1", Content: &content},
	}}); err != nil {
		t.Fatal(err)
	}
	term := execenvtest.OpenShell(t, env, fence, "sh")
	go func() { _, _ = io.Copy(io.Discard, term) }()
	if _, err := term.Write([]byte("cat hello.txt > seen.txt\n")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		page, err := env.Observe(t.Context(), fence, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(page.Observations) - 1; i >= 0; i-- {
			event := page.Observations[i]
			if event.Path != "seen.txt" || event.Content == nil {
				continue
			}
			reader, err := env.OpenObservationContent(t.Context(), fence, *event.Content)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(reader)
			_ = reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) == harvestBody {
				return
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the guest-written body was not harvested")
}

func freeLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func startHostProcess(t *testing.T, bin, configPath, addr string) {
	t.Helper()
	cmd := exec.Command(bin, "-config", configPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("host process did not listen")
}

func harvestTLS(t *testing.T, certPath string) *tls.Config {
	t.Helper()
	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("bootstrap TLS cert")
	}
	return &tls.Config{RootCAs: pool}
}

func startCertifyHost(t *testing.T) *remote.Client {
	t.Helper()
	if os.Getenv("EXECENV_ISOLATION") != "1" {
		t.Skip("set EXECENV_ISOLATION=1 on a machine with isolation hardware")
	}
	if runtime.GOOS != "linux" {
		t.Skip("certify is linux-only")
	}
	kernel := os.Getenv("EXECENV_FIXTURE_KERNEL")
	rootfs := os.Getenv("EXECENV_FIXTURE_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set EXECENV_FIXTURE_KERNEL and EXECENV_FIXTURE_ROOTFS")
	}
	hash, err := isolated.Digest(kernel, rootfs)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	stub := filepath.Join(root, "execenv")
	build := exec.Command("go", "build", "-o", stub, "github.com/sudosylabs/execenv/cmd/execenv")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build execenv: %v\n%s", err, out)
	}
	opts := ctl.Options{
		Prefix:   filepath.Join(root, "usr", "local"),
		Sysconf:  filepath.Join(root, "etc", "execenv"),
		State:    filepath.Join(root, "var", "lib", "execenv"),
		Listen:   "127.0.0.1:0",
		Capacity: daemon.Resources{CPUMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 2 << 30},
		Execenv:  stub,
		NoStart:  true,
		NoFetch:  true,
	}
	if err := ctl.Bootstrap(opts, io.Discard); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	path := filepath.Join(opts.Sysconf, "host.json")
	cfg, err := daemon.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	addr := freeLoopback(t)
	cfg.Listen = addr
	cfg.Images = []daemon.Image{{
		ID:     "default",
		Kernel: kernel,
		Rootfs: rootfs,
		Hash:   hash,
	}}
	if err := daemon.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = daemon.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	startHostProcess(t, filepath.Join(opts.Prefix, "bin", "execenv"), path, addr)
	client, err := remote.New(remote.Config{
		Address:  addr,
		Security: remote.SecurityTLS,
		Token:    []byte(cfg.Token),
		TLS:      harvestTLS(t, cfg.TLSCert),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if report, err := client.Ready(t.Context()); err != nil || report.Isolation != execenv.IsolationVM {
		t.Fatalf("Ready() isolation = %q, %v; want %q", report.Isolation, err, execenv.IsolationVM)
	}

	return client
}
