package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	t.Parallel()
	cmd := newRoot()
	cmd.SetArgs([]string{"--version"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "version=") || !strings.Contains(got, "build=") || !strings.Contains(got, "tag=") {
		t.Fatalf("--version = %q", got)
	}
	if strings.Contains(got, "token") || strings.Contains(got, "hash") {
		t.Fatalf("--version leaked a secret: %q", got)
	}
}

func TestBootstrapCommandFailsClosedWithoutDevice(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cmd := newRoot()
	cmd.SetArgs([]string{
		"bootstrap",
		"--device", filepath.Join(root, "no-kvm"),
		"--prefix", filepath.Join(root, "usr", "local"),
		"--sysconf", filepath.Join(root, "etc", "execenv"),
		"--state", filepath.Join(root, "var", "lib", "execenv"),
		"--execenv", filepath.Join(root, "missing"),
		"--no-start",
		"--no-fetch",
		"--insecure",
	})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("bootstrap succeeded without an isolation device")
	}
	msg := err.Error() + errb.String()
	if !strings.Contains(msg, "isolation device") || !strings.Contains(msg, "no container fallback") {
		t.Fatalf("error = %v (%s)", err, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q", out.String())
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc", "execenv", "host.json")); !os.IsNotExist(statErr) {
		t.Fatal("wrote config before the device check")
	}
}

func TestBootstrapCommandDoesNotPrintSecrets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bindir := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	execenvBin := filepath.Join(bindir, "execenv")
	for _, name := range []string{"execenv", "firecracker", "jailer"} {
		if err := os.WriteFile(filepath.Join(bindir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	device := filepath.Join(root, "kvm")
	if err := os.WriteFile(device, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}
	sysconf := filepath.Join(root, "etc", "execenv")
	cmd := newRoot()
	cmd.SetArgs([]string{
		"bootstrap",
		"--device", device,
		"--prefix", filepath.Join(root, "usr", "local"),
		"--sysconf", sysconf,
		"--state", filepath.Join(root, "var", "lib", "execenv"),
		"--execenv", execenvBin,
		"--listen", "127.0.0.1:8443",
		"--slots", "4",
		"--no-start",
		"--no-fetch",
		"--insecure",
	})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bootstrap: %v\n%s", err, errb.String())
	}
	raw, err := os.ReadFile(filepath.Join(sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Token is the only secret in a first-run empty catalog.
	if !bytes.Contains(raw, []byte(`"token"`)) {
		t.Fatalf("config = %s", raw)
	}
	combined := out.String() + errb.String()
	// A 64-hex token in config must not appear on either stream.
	body := string(raw)
	start := strings.Index(body, `"token": "`)
	if start < 0 {
		t.Fatal("config missing token")
	}
	start += len(`"token": "`)
	end := strings.Index(body[start:], `"`)
	token := body[start : start+end]
	if token == "" || strings.Contains(combined, token) {
		t.Fatalf("token missing or leaked: stdout=%q stderr=%q", out.String(), errb.String())
	}
}
