package ctl

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv/daemon"
)

func TestBootstrapFailsClosedWithoutDevice(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	opts.Device = filepath.Join(root, "no-kvm")

	var out bytes.Buffer
	err := Bootstrap(opts, &out)
	if err == nil {
		t.Fatal("Bootstrap() succeeded without an isolation device")
	}
	msg := err.Error()
	if !strings.Contains(msg, "isolation device") || !strings.Contains(msg, "no container fallback") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(opts.Sysconf, "host.json")); !os.IsNotExist(statErr) {
		t.Fatal("wrote config before the device check")
	}
}

func TestBootstrapRequiresRuntimeAndSupervisor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	// Runtime is present; supervisor is not. NoFetch must not invent a container host.
	if err := os.Remove(filepath.Join(opts.Prefix, "bin", supervisorName)); err != nil {
		t.Fatal(err)
	}

	err := Bootstrap(opts, io.Discard)
	if err == nil {
		t.Fatal("Bootstrap() succeeded without the supervisor pair")
	}
	if !strings.Contains(err.Error(), "supervisor") {
		t.Fatalf("error = %v", err)
	}
}

func TestBootstrapWritesConfigWithoutLeakingSecrets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)

	var first bytes.Buffer
	if err := Bootstrap(opts, &first); err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	stdout := first.String()
	cfg, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token == "" {
		t.Fatal("token missing from config")
	}
	if strings.Contains(stdout, cfg.Token) {
		t.Fatal("stdout printed the token")
	}
	if !strings.Contains(stdout, "token=written to config") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "hash=none") {
		t.Fatalf("stdout claimed a hash: %q", stdout)
	}
	if cfg.Adapter != "isolated" || cfg.Slots != 8 {
		t.Fatalf("config = %+v", cfg)
	}
	if len(cfg.Images) != 0 {
		t.Fatalf("bootstrap installed images = %v", cfg.Images)
	}

	info, err := os.Stat(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("host.json mode = %o", info.Mode().Perm())
	}

	firstToken := cfg.Token
	var second bytes.Buffer
	if err := Bootstrap(opts, &second); err != nil {
		t.Fatalf("second Bootstrap() error = %v", err)
	}
	again, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != firstToken {
		t.Fatal("re-run rotated the token")
	}

	unit := filepath.Join(filepath.Dir(opts.Sysconf), "systemd", "system", "execenv.service")
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "-config "+filepath.Join(opts.Sysconf, "host.json")) {
		t.Fatalf("unit = %s", body)
	}
	if strings.Contains(string(body), firstToken) {
		t.Fatal("systemd unit contains the token")
	}
}

func TestBootstrapKeepsExistingImages(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.Sysconf, "host.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A later install ticket writes real disks. Re-run must not wipe them.
	injected := strings.Replace(string(raw), `"images": []`, `"images": [{"id":"python","kernel":"/k","rootfs":"/r","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`, 1)
	if err := os.WriteFile(path, []byte(injected), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Bootstrap(opts, &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := daemon.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Images) != 1 || cfg.Images[0].ID != "python" {
		t.Fatalf("images = %+v", cfg.Images)
	}
	if strings.Contains(out.String(), cfg.Images[0].Hash) || strings.Contains(out.String(), cfg.Token) {
		t.Fatalf("stdout leaked hash or token: %q", out.String())
	}
	if !strings.Contains(out.String(), "hash=written to config") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestBootstrapKeepsAllowAndResources(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(opts.Sysconf, "host.json")
	cfg, err := daemon.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Network = "allowlist"
	cfg.Allow = []string{"203.0.113.8"}
	cfg.CPUMillis = 2000
	cfg.MemoryBytes = 1 << 31
	cfg.DiskBytes = 40 << 30
	if err := daemon.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	again, err := daemon.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Token != cfg.Token {
		t.Fatal("re-run rotated the token")
	}
	if again.Network != "allowlist" || len(again.Allow) != 1 || again.Allow[0] != "203.0.113.8" {
		t.Fatalf("allow stripped: %+v", again)
	}
	if again.CPUMillis != 2000 || again.MemoryBytes != 1<<31 || again.DiskBytes != 40<<30 {
		t.Fatalf("resources stripped: %+v", again)
	}
}

func TestBootstrapKeepsTLSOnRerun(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	opts.Insecure = false
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(opts.Sysconf, "host.json")
	first, err := daemon.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if first.Security != "tls" || first.TLSCert == "" {
		t.Fatalf("first config = %+v", first)
	}
	pemBytes, err := os.ReadFile(first.TLSCert)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("TLS cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "execenv" {
		t.Fatalf("DNSNames = %v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("IPAddresses = %v", cert.IPAddresses)
	}
	opts.Insecure = true
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	again, err := daemon.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if again.Security != "tls" || again.TLSCert != first.TLSCert || again.Token != first.Token {
		t.Fatalf("re-run dropped TLS or token: %+v", again)
	}
}

func testOpts(t *testing.T, root string) Options {
	t.Helper()
	bindir := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	execenvBin := filepath.Join(bindir, "execenv")
	for _, name := range []string{"execenv", runtimeName, supervisorName} {
		if err := os.WriteFile(filepath.Join(bindir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	device := filepath.Join(root, "kvm")
	if err := os.WriteFile(device, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}
	return Options{
		Prefix:   filepath.Join(root, "usr", "local"),
		Sysconf:  filepath.Join(root, "etc", "execenv"),
		State:    filepath.Join(root, "var", "lib", "execenv"),
		Device:   device,
		Listen:   "127.0.0.1:8443",
		Slots:    8,
		Execenv:  execenvBin,
		NoStart:  true,
		NoFetch:  true,
		Insecure: true,
	}
}

func TestBootstrapCorruptConfigDoesNotLeakToken(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := os.MkdirAll(opts.Sysconf, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := "super-secret-token"
	bad := []byte(`{"token": "` + secret + `",`)
	if err := os.WriteFile(filepath.Join(opts.Sysconf, "host.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Bootstrap(opts, io.Discard)
	if err == nil {
		t.Fatal("Bootstrap() accepted corrupt config")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked token: %v", err)
	}
}
