package ctl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv/daemon"
)

func TestUpgradeReplacesBinariesAndLeavesHost(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	execenvNew := linuxAMD64ELF("execenv-v2")
	ctlNew := linuxAMD64ELF("ctl-v2")
	srv := newReleaseServer(t, map[string][]byte{
		"execenv":    execenvNew,
		"execenvctl": ctlNew,
		"SHA256SUMS": checksumFile(map[string][]byte{
			"execenv":    execenvNew,
			"execenvctl": ctlNew,
		}),
	})
	opts.ReleaseURL = srv.URL
	reloads := 0
	opts.Reload = func() error { reloads++; return nil }

	var out bytes.Buffer
	if err := Upgrade(opts, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "upgraded=execenv,execenvctl") {
		t.Fatalf("stdout = %q", out.String())
	}
	if strings.Contains(out.String(), before.Token) {
		t.Fatal("stdout leaked token")
	}
	if reloads != 1 {
		t.Fatalf("reload = %d", reloads)
	}
	got, err := os.ReadFile(filepath.Join(opts.Prefix, "bin", "execenv"))
	if err != nil || !bytes.Equal(got, execenvNew) {
		t.Fatalf("execenv not replaced")
	}
	got, err = os.ReadFile(filepath.Join(opts.Prefix, "bin", "execenvctl"))
	if err != nil || !bytes.Equal(got, ctlNew) {
		t.Fatalf("execenvctl not replaced")
	}
	after, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Token != before.Token || after.TLSCert != before.TLSCert {
		t.Fatal("upgrade mutated host config")
	}

	var again bytes.Buffer
	if err := Upgrade(opts, &again); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.String(), "already current") {
		t.Fatalf("stdout = %q", again.String())
	}
	if reloads != 1 {
		t.Fatalf("reload on current = %d", reloads)
	}
}

func TestUpgradeRefusesChecksumMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := os.MkdirAll(opts.binDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	good := linuxAMD64ELF("good")
	bad := linuxAMD64ELF("bad")
	srv := newReleaseServer(t, map[string][]byte{
		"execenv":    bad,
		"execenvctl": good,
		"SHA256SUMS": checksumFile(map[string][]byte{
			"execenv":    good,
			"execenvctl": good,
		}),
	})
	opts.ReleaseURL = srv.URL
	err := Upgrade(opts, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpgradeRefusesWrongArch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := os.MkdirAll(opts.binDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	arm := linuxAMD64ELF("arm")
	arm[18] = elfMachineARM
	arm[19] = 0
	srv := newReleaseServer(t, map[string][]byte{
		"execenv":    arm,
		"execenvctl": linuxAMD64ELF("ctl"),
		"SHA256SUMS": checksumFile(map[string][]byte{
			"execenv":    arm,
			"execenvctl": linuxAMD64ELF("ctl"),
		}),
	})
	opts.ReleaseURL = srv.URL
	err := Upgrade(opts, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("error = %v", err)
	}
}

func TestUpgradeRefusesNonLinux(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := os.MkdirAll(opts.binDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	macho := []byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0, 0, 0}
	srv := newReleaseServer(t, map[string][]byte{
		"execenv":    macho,
		"execenvctl": linuxAMD64ELF("ctl"),
		"SHA256SUMS": checksumFile(map[string][]byte{
			"execenv":    macho,
			"execenvctl": linuxAMD64ELF("ctl"),
		}),
	})
	opts.ReleaseURL = srv.URL
	err := Upgrade(opts, io.Discard)
	if err == nil {
		t.Fatal("Upgrade() accepted a non-linux binary")
	}
	if !strings.Contains(err.Error(), "non-linux") {
		t.Fatalf("error = %v", err)
	}
}

func linuxAMD64ELF(tag string) []byte {
	raw := make([]byte, 64)
	raw[0], raw[1], raw[2], raw[3] = 0x7f, 'E', 'L', 'F'
	raw[4] = elfClass64
	raw[5] = elfData2LSB
	copy(raw[40:], tag)
	raw[18] = elfMachineX64
	return raw
}

func checksumFile(files map[string][]byte) []byte {
	var b strings.Builder
	for name, body := range files {
		sum := sha256.Sum256(body)
		b.WriteString(hex.EncodeToString(sum[:]))
		b.WriteString("  ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}
