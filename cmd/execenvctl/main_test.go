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

func TestBootstrapCommandRejectsARegularFileAsDevice(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	device := filepath.Join(root, "kvm")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newRoot()
	cmd.SetArgs([]string{"bootstrap", "--device", device, "--sysconf", root, "--no-start"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "isolation device") {
		t.Fatalf("bootstrap with a regular file: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "host.json")); !os.IsNotExist(err) {
		t.Fatalf("config exists after failed preflight: %v", err)
	}
}

func TestRestartingCommandsTakeForceAndDrainExists(t *testing.T) {
	t.Parallel()
	root := newRoot()
	for _, name := range []string{"install", "remove", "upgrade"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("%s: %v", name, err)
		}
		if cmd.Flags().Lookup("force") == nil {
			t.Fatalf("%s has no --force", name)
		}
		if !strings.Contains(cmd.Long, "drain") {
			t.Fatalf("%s help does not mention drain", name)
		}
	}
	drain, _, err := root.Find([]string{"drain"})
	if err != nil || drain.Name() != "drain" || drain.Flags().Lookup("timeout") == nil {
		t.Fatalf("drain command: %v", err)
	}
	if undrain, _, err := root.Find([]string{"undrain"}); err != nil || undrain.Name() != "undrain" {
		t.Fatalf("undrain command: %v", err)
	}
}
