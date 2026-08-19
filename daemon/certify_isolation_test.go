//go:build isolation

package daemon_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/daemon"
	"github.com/sudosylabs/execenv/internal/ctl"
	"github.com/sudosylabs/execenv/isolated"
	"github.com/sudosylabs/execenv/remote"
)

const harvestBody = "from-host\n"

// TestCertifyRemoteHarvestsGuestFile is the certification path: a
// bootstrapped host process, the client is remote.New, Attach is a guest
// shell, and a guest-created file is harvested. Not an echo PTY.
func TestCertifyRemoteHarvestsGuestFile(t *testing.T) {
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
		Prefix:  filepath.Join(root, "usr", "local"),
		Sysconf: filepath.Join(root, "etc", "execenv"),
		State:   filepath.Join(root, "var", "lib", "execenv"),
		Listen:  "127.0.0.1:0",
		Slots:   1,
		Execenv: stub,
		NoStart: true,
		NoFetch: true,
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

	env, err := client.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Revoke(t.Context(), "grant-1") })
	if err := env.ReplaceTree(t.Context(), execenv.Tree{{
		Path:    "hello.txt",
		Kind:    execenv.KindFile,
		Version: "v1",
		Data:    []byte(harvestBody),
	}}); err != nil {
		t.Fatal(err)
	}
	obs, err := env.Watch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	term, err := env.Attach(t.Context(), execenv.Window{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte("cat hello.txt > seen.txt\n")); err != nil {
		t.Fatal(err)
	}

	deadline, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for {
		ev, err := obs.Next(deadline)
		if err != nil {
			t.Fatalf("Watch did not see guest create: %v", err)
		}
		if ev.Path == "seen.txt" && (ev.Op == execenv.OpCreate || ev.Op == execenv.OpReplace) {
			break
		}
	}
	for deadline.Err() == nil {
		body, err := env.Open(t.Context(), "seen.txt")
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		got, readErr := io.ReadAll(body)
		closeErr := body.Close()
		if readErr != nil || closeErr != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		got = bytes.ReplaceAll(got, []byte("\r"), nil)
		if string(got) == harvestBody {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("Open did not harvest the guest-written body")
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
