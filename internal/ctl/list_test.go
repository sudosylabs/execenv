package ctl

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/daemon"
	"github.com/sudosylabs/execenv/isolated"
)

func TestListShowsInstalledAndAvailable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := bootstrappedWithFixture(t, root)

	var out bytes.Buffer
	if err := List(opts, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "installed=fixture") {
		t.Fatalf("stdout = %q", text)
	}
	if !strings.Contains(text, "available=fixture") {
		t.Fatalf("stdout = %q", text)
	}
	cfg, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, cfg.Token) || strings.Contains(text, cfg.Images[0].Hash) {
		t.Fatalf("list leaked secret: %q", text)
	}
}

func TestRemoveDropsDiskAndConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := bootstrappedWithFixture(t, root)
	cfg, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	rootfs := cfg.Images[0].Rootfs
	token := cfg.Token
	reloads := 0
	opts.Reload = func() error { reloads++; return nil }

	var out bytes.Buffer
	if err := Remove(opts, "fixture", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "removed=fixture") {
		t.Fatalf("stdout = %q", out.String())
	}
	if _, err := os.Stat(rootfs); !os.IsNotExist(err) {
		t.Fatal("rootfs still on disk")
	}
	after, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Token != token {
		t.Fatal("remove changed the token")
	}
	if len(after.Images) != 0 {
		t.Fatalf("images = %+v", after.Images)
	}
	if reloads != 1 {
		t.Fatalf("reload = %d", reloads)
	}
	if err := Remove(opts, "fixture", io.Discard); err != nil {
		t.Fatal(err)
	}
	images := make([]isolated.Image, 0, len(after.Images))
	for _, img := range after.Images {
		images = append(images, isolated.Image{
			ID:     execenv.Image(img.ID),
			Kernel: img.Kernel,
			Rootfs: img.Rootfs,
			Hash:   img.Hash,
		})
	}
	host, err := isolated.New(isolated.Config{
		WorkDir: filepath.Join(root, "ready"),
		Images:  images,
		Slots:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Images) != 0 {
		t.Fatalf("Ready() Images = %v", report.Images)
	}
}



func TestRemoveKeepsOtherIDs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	kernel := []byte("k")
	a := []byte("a-rootfs")
	b := []byte("b-rootfs")
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	sumA := writeDigest(t, filepath.Join(root, "a"), kernel, a)
	if err := os.MkdirAll(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	sumB := writeDigest(t, filepath.Join(root, "b"), kernel, b)
	srv := newReleaseServer(t, map[string][]byte{
		"index.json": []byte(`{"kernel":"vmlinux","images":[` +
			`{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"` + sumA + `"},` +
			`{"id":"other","rootfs":"rootfs-other.ext4","hash":"` + sumB + `"}]}`),
		"vmlinux":             kernel,
		"rootfs-fixture.ext4": a,
		"rootfs-other.ext4":   b,
	})
	opts.ReleaseURL = srv.URL
	opts.Reload = func() error { return nil }
	if err := Install(opts, "fixture", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := Install(opts, "other", io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := Remove(opts, "fixture", io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Images) != 1 || cfg.Images[0].ID != "other" {
		t.Fatalf("images = %+v", cfg.Images)
	}
	if _, err := os.Stat(cfg.Images[0].Rootfs); err != nil {
		t.Fatal(err)
	}
}

func TestStatusOmitsSecrets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	opts := bootstrappedWithFixture(t, root)
	var out bytes.Buffer
	if err := Status(opts, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "device=ok") || !strings.Contains(text, "installed=fixture") {
		t.Fatalf("stdout = %q", text)
	}
	cfg, err := daemon.Load(filepath.Join(opts.Sysconf, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, cfg.Token) || strings.Contains(text, cfg.Images[0].Hash) {
		t.Fatalf("status leaked secret: %q", text)
	}
}

func bootstrappedWithFixture(t *testing.T, root string) Options {
	t.Helper()
	opts := testOpts(t, root)
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	kernel := []byte("kernel-bytes")
	rootfs := []byte("rootfs-bytes")
	sum := writeDigest(t, root, kernel, rootfs)
	srv := newReleaseServer(t, map[string][]byte{
		"index.json":          []byte(`{"kernel":"vmlinux","images":[{"id":"fixture","rootfs":"rootfs-fixture.ext4","hash":"` + sum + `"}]}`),
		"vmlinux":             kernel,
		"rootfs-fixture.ext4": rootfs,
	})
	opts.ReleaseURL = srv.URL
	opts.Reload = func() error { return nil }
	if err := Install(opts, "fixture", io.Discard); err != nil {
		t.Fatal(err)
	}
	return opts
}
